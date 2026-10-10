package room

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"

	"kam.bytecoder.nl/internal/db"
	"kam.bytecoder.nl/internal/kiss"
	"kam.bytecoder.nl/internal/mesh"
	"kam.bytecoder.nl/internal/models"
)

// loggedIn returns a peer logged in with the password and its response consumed.
func (h *harness) loggedIn(seed byte, password string) *peer {
	h.t.Helper()
	p := h.newPeer(seed)
	h.login(p, password, 0, floodVia())
	return p
}

func TestPostIsCommittedBeforeItIsAcknowledged(t *testing.T) {
	h := newHarness(t, nil)
	p := h.loggedIn(1, "guest-pass")

	var storedWhenAcked int64 = -1
	h.tx.onSubmit = func(sentPacket) { storedWhenAcked = h.postCount() }
	pkt, plain := p.text("hello room", floodVia())
	h.svc.process(pkt, h.now)
	ack := h.tx.only(t)

	if storedWhenAcked != 1 {
		t.Fatalf("the ACK was queued when %d posts were stored; it must follow the commit", storedWhenAcked)
	}
	if ack.Pkt.PayloadType() != mesh.PayloadAck || len(ack.Pkt.Payload) != 4 {
		t.Fatalf("expected a plain 4-byte ACK, got %+v", ack.Pkt)
	}
	want := mesh.AckHash(plain[:5+len("hello room")], p.pubSlice())
	if !bytes.Equal(ack.Pkt.Payload, want[:]) {
		t.Fatalf("ACK %x, want %x", ack.Pkt.Payload, want)
	}
	if ack.Req.Delay != txtAckDelay || ack.Req.Priority != kiss.TxPriorityFloodReply || !ack.Pkt.IsFlood() {
		t.Fatalf("unknown route: ACK should be a flood reply after %v: %+v", txtAckDelay, ack.Req)
	}
	posts, _ := h.repo.LatestPosts(testContext(), "lobby", 10)
	if len(posts) != 1 || posts[0].Text != "hello room" || !bytes.Equal(posts[0].Author, p.pubSlice()) || posts[0].Timestamp != uint32(h.now.Unix())+1 {
		t.Fatalf("stored (the login response used the first timestamp of this second): %+v", posts)
	}
	if h.svc.Stats().PostsAccepted != 1 {
		t.Fatalf("stats: %+v", h.svc.Stats())
	}
}

func TestPostAckIsDirectOnceTheRouteIsKnown(t *testing.T) {
	h := newHarness(t, nil)
	p := h.loggedIn(1, "guest-pass")
	h.svc.process(p.pathPacket([]byte{0xAA, 0xBB, 0xCC}, 1, mesh.PayloadAck, []byte{0, 0, 0, 0}, direct()), h.now)
	h.tx.none(t)

	pkt, _ := p.text("via route", direct())
	h.svc.process(pkt, h.now)
	ack := h.tx.only(t)
	if !ack.Pkt.IsDirect() || ack.Pkt.PathLen != 0x03 || !bytes.Equal(ack.Pkt.Path, []byte{0xAA, 0xBB, 0xCC}) || ack.Req.Priority != kiss.TxPriorityReply || ack.Req.Delay != txtAckDelay {
		t.Fatalf("ACK should follow the stored route: %+v", ack)
	}
}

func TestPostRetryIsAcknowledgedAgainButStoredOnce(t *testing.T) {
	h := newHarness(t, nil)
	p := h.loggedIn(1, "guest-pass")
	ts := p.tick()
	first, plain1 := p.textAt(ts, mesh.TxtTypePlain, 0, "once", floodVia())
	retry, plain2 := p.textAt(ts, mesh.TxtTypePlain, 2, "once", floodVia()) // new attempt bits: different ciphertext

	h.svc.process(first, h.now)
	ack1 := h.tx.only(t)
	h.svc.process(retry, h.now)
	ack2 := h.tx.only(t)

	if h.postCount() != 1 {
		t.Fatalf("%d posts stored, want 1", h.postCount())
	}
	// The ACK covers timestamp, flags and text, so the retry's ACK differs.
	want1, want2 := mesh.AckHash(plain1, p.pubSlice()), mesh.AckHash(plain2, p.pubSlice())
	if !bytes.Equal(ack1.Pkt.Payload, want1[:]) || !bytes.Equal(ack2.Pkt.Payload, want2[:]) {
		t.Fatalf("ACKs %x %x, want %x %x", ack1.Pkt.Payload, ack2.Pkt.Payload, want1, want2)
	}

	// An older timestamp is a replay and gets nothing.
	old, _ := p.textAt(ts-1, mesh.TxtTypePlain, 0, "stale", floodVia())
	h.svc.process(old, h.now)
	h.tx.none(t)
	if h.postCount() != 1 {
		t.Fatal("replayed post stored")
	}
}

func TestFailedWriteSendsNoAckAndAllowsTheRetry(t *testing.T) {
	var flaky *flakyStore
	h := newHarnessWith(t, harnessOptions{store: func(s Store) Store { flaky = &flakyStore{Store: s}; return flaky }})
	p := h.loggedIn(1, "guest-pass")
	ts := p.tick()
	pkt, _ := p.textAt(ts, mesh.TxtTypePlain, 0, "important", floodVia())

	flaky.appendErr = errors.New("disk full")
	h.svc.process(pkt, h.now)
	h.tx.none(t)
	if h.postCount() != 0 || h.svc.Stats().PostsRejected != 1 {
		t.Fatalf("posts=%d stats=%+v", h.postCount(), h.svc.Stats())
	}
	if len(h.room.posts) != 0 {
		t.Fatal("an uncommitted post reached the push cache")
	}

	// The client retries with the same timestamp; it must not be mistaken for a duplicate.
	flaky.appendErr = nil
	retry, _ := p.textAt(ts, mesh.TxtTypePlain, 1, "important", floodVia())
	h.svc.process(retry, h.now)
	h.tx.only(t)
	if h.postCount() != 1 {
		t.Fatalf("the retried post was lost: %d stored", h.postCount())
	}
}

func TestOnlyReadWriteAndAdminMayPost(t *testing.T) {
	cfg := testRoomConfig()
	cfg.AllowReadOnly = true
	first := newHarnessWith(t, harnessOptions{rooms: []models.RoomConfig{cfg}})
	readOnly := first.newPeer(2)
	if err := first.repo.SaveClients(testContext(), "lobby", []db.RoomClient{{PublicKey: readOnly.pubSlice(), Role: RoleReadOnly, OutPathLen: 0xFF}}); err != nil {
		t.Fatal(err)
	}
	// A restart restores the read-only client; no login path can create that role.
	h := newHarnessWith(t, harnessOptions{rooms: []models.RoomConfig{cfg}, repo: first.repo})
	if c := h.room.byKey[readOnly.pub()]; c == nil || c.role != RoleReadOnly {
		t.Fatal("the read-only client was not restored")
	}
	guest := h.newPeer(1)
	h.login(guest, "anything", 0, floodVia())
	if h.clientFor(guest).role != RoleGuest {
		t.Fatalf("role %d", h.clientFor(guest).role)
	}

	for name, p := range map[string]*peer{"guest": guest, "read-only": readOnly} {
		pkt, _ := p.text("may I?", floodVia())
		h.svc.process(pkt, h.now)
		h.tx.none(t)
		if h.postCount() != 0 {
			t.Fatalf("%s posted", name)
		}
	}
	if h.svc.Stats().PostsRejected != 2 {
		t.Fatalf("stats: %+v", h.svc.Stats())
	}

	admin := h.newPeer(3)
	h.login(admin, "admin-pass", 0, floodVia())
	pkt, _ := admin.text("admin post", floodVia())
	h.svc.process(pkt, h.now)
	h.tx.only(t)
	if h.postCount() != 1 {
		t.Fatal("admin could not post")
	}
}

func TestPostTextHandling(t *testing.T) {
	cases := []struct {
		name   string
		text   string
		stored string // empty: nothing stored
	}{
		{"short text", "hi", "hi"},
		{"exactly 150 bytes", strings.Repeat("a", 150), strings.Repeat("a", 150)},
		{"151 bytes is cut to 150", strings.Repeat("a", 151), strings.Repeat("a", 150)},
		{"long text is cut to 150", strings.Repeat("b", 170), strings.Repeat("b", 150)},
		{"a multi-byte rune is never split", strings.Repeat("a", 149) + "é" + "tail", strings.Repeat("a", 149)},
		{"multi-byte text that fits is kept", strings.Repeat("日", 50), strings.Repeat("日", 50)},
		{"empty text is acknowledged but not stored", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			p := h.loggedIn(1, "guest-pass")
			// Long texts must still fit one packet: 5 + 170 bytes pads to 176.
			pkt, plain := p.text(tc.text, floodVia())
			h.svc.process(pkt, h.now)
			ack := h.tx.only(t)
			want := mesh.AckHash(plain[:5+len(tc.text)], p.pubSlice())
			if !bytes.Equal(ack.Pkt.Payload, want[:]) {
				t.Fatalf("the ACK must cover the text as received: %x vs %x", ack.Pkt.Payload, want)
			}
			posts, _ := h.repo.LatestPosts(testContext(), "lobby", 10)
			if tc.stored == "" {
				if len(posts) != 0 {
					t.Fatalf("stored %+v", posts)
				}
				return
			}
			if len(posts) != 1 || posts[0].Text != tc.stored {
				t.Fatalf("stored %+v, want %q", posts, tc.stored)
			}
		})
	}
}

func TestTextStopsAtTheFirstNulIgnoringPadding(t *testing.T) {
	h := newHarness(t, nil)
	p := h.loggedIn(1, "guest-pass")
	pkt, plain := p.text("visible\x00hidden", floodVia())
	h.svc.process(pkt, h.now)
	ack := h.tx.only(t)
	want := mesh.AckHash(plain[:5+len("visible")], p.pubSlice())
	if !bytes.Equal(ack.Pkt.Payload, want[:]) {
		t.Fatalf("ACK %x, want %x", ack.Pkt.Payload, want)
	}
	if posts, _ := h.repo.LatestPosts(testContext(), "lobby", 10); len(posts) != 1 || posts[0].Text != "visible" {
		t.Fatalf("stored %+v", posts)
	}
}

func TestUnsupportedTextTypesGetNoReply(t *testing.T) {
	h := newHarness(t, nil)
	admin := h.loggedIn(1, "admin-pass")
	for name, kind := range map[string]byte{"CLI data": mesh.TxtTypeCLIData, "signed plain": mesh.TxtTypeSignedPlain, "unknown": 9} {
		pkt, _ := admin.textAt(admin.tick(), kind, 0, "ver", floodVia())
		h.svc.process(pkt, h.now)
		h.tx.none(t)
		if h.postCount() != 0 {
			t.Fatalf("%s created a post", name)
		}
	}
}

func TestPostTimestampsAreStrictlyIncreasingAndSurviveAClockStep(t *testing.T) {
	h := newHarness(t, nil)
	p := h.loggedIn(1, "guest-pass")
	for i := 0; i < 3; i++ {
		pkt, _ := p.text("same second", floodVia())
		h.svc.process(pkt, h.now)
		h.tx.only(t)
	}
	posts, _ := h.repo.LatestPosts(testContext(), "lobby", 10)
	// The login response consumed the first timestamp of this second.
	base := uint32(testEpoch.Unix())
	if len(posts) != 3 || posts[0].Timestamp != base+1 || posts[1].Timestamp != base+2 || posts[2].Timestamp != base+3 {
		t.Fatalf("timestamps %+v, want %d..%d", posts, base+1, base+3)
	}

	// After a restart the unique counter starts over. A clock that stepped back
	// can hit stored timestamps; the post must still get a free one.
	restarted := newHarnessWith(t, harnessOptions{repo: h.repo})
	restarted.now = time.Unix(int64(base)+1, 0)
	q := restarted.newPeer(2)
	restarted.login(q, "guest-pass", 0, floodVia())
	pkt, _ := q.text("after the step", floodVia())
	restarted.svc.process(pkt, restarted.now)
	restarted.tx.only(t)
	all, _ := h.repo.LatestPosts(testContext(), "lobby", 10)
	if len(all) != 4 || all[3].Timestamp != base+4 {
		t.Fatalf("the post after a clock step must take the next free timestamp: %+v", all)
	}
}

func TestPostsReachOtherClientsOnlyAfterTheSyncDelay(t *testing.T) {
	h := newHarness(t, nil)
	author := h.loggedIn(1, "guest-pass")
	reader := h.loggedIn(2, "guest-pass")

	pkt, _ := author.text("news", floodVia())
	h.svc.process(pkt, h.now)
	h.tx.only(t)
	postTS := uint32(h.now.Unix()) + 2 // after the two login responses

	// Poll in small steps: the post must not be pushed before it is six seconds old.
	var push sentPacket
	var pushedAt time.Time
	for i := 0; i < 200 && push.Req.Raw == nil; i++ {
		h.advance(100 * time.Millisecond)
		if got := h.tx.take(); len(got) > 0 {
			push, pushedAt = got[0], h.now
		}
	}
	if push.Req.Raw == nil {
		t.Fatal("the post was never pushed")
	}
	if age := pushedAt.Unix() - int64(postTS); age < postSyncDelaySecs {
		t.Fatalf("pushed when the post was %d s old; the minimum is %d", age, postSyncDelaySecs)
	}

	if push.Pkt.PayloadType() != mesh.PayloadTxtMsg || push.Req.Priority != kiss.TxPriorityPush || !push.Pkt.IsFlood() {
		t.Fatalf("with no route the push is a flood: %+v", push)
	}
	if push.Req.Tag != h.room.pushTag(h.clientFor(reader)) {
		t.Fatalf("push tag %q", push.Req.Tag)
	}
	plain := reader.open(push)
	if binary.LittleEndian.Uint32(plain[0:4]) != postTS {
		t.Fatalf("post timestamp %d, want %d", binary.LittleEndian.Uint32(plain[0:4]), postTS)
	}
	if plain[4]>>2 != mesh.TxtTypeSignedPlain || !bytes.Equal(plain[5:9], author.pubSlice()[:4]) || string(plain[9:13]) != "news" {
		t.Fatalf("push plaintext %x", plain)
	}
	if len(push.Pkt.Payload) > mesh.MaxPacketPayload {
		t.Fatal("push does not fit one packet")
	}
	c := h.clientFor(reader)
	if !c.hasPending || c.ackDeadline.Sub(h.now) != pushAckTimeoutFlood {
		t.Fatalf("pending=%v deadline in %v, want a 12 s flood timeout", c.hasPending, c.ackDeadline.Sub(h.now))
	}
	want := mesh.AckHash(plain[:9+len("news")], reader.pubSlice())
	if c.pendingAck != want {
		t.Fatalf("expected ACK %x, recorded %x", want, c.pendingAck)
	}

	// The author never receives their own post.
	for i := 0; i < 10; i++ {
		h.advance(1500 * time.Millisecond)
		for _, sp := range h.tx.take() {
			if sp.Req.Tag == h.room.pushTag(h.clientFor(author)) {
				t.Fatal("the post was pushed back to its author")
			}
		}
	}
}

func TestAckAdvancesTheCursorAndPushesTheNextPostOldestFirst(t *testing.T) {
	h := newHarness(t, nil)
	author := h.loggedIn(1, "guest-pass")
	reader := h.loggedIn(2, "guest-pass")
	for _, text := range []string{"first", "second", "third"} {
		pkt, _ := author.text(text, floodVia())
		h.svc.process(pkt, h.now)
		h.tx.only(t)
		h.advance(time.Second)
	}
	h.advance(10 * time.Second)

	var received []string
	var lastPush time.Time
	for len(received) < 3 {
		pushes := h.tx.take()
		if len(pushes) > 1 {
			t.Fatalf("%d pushes in flight to one client", len(pushes))
		}
		for _, sp := range pushes {
			plain := reader.open(sp)
			received = append(received, string(bytes.TrimRight(plain[9:], "\x00")))
			if !lastPush.IsZero() && h.now.Sub(lastPush) < syncPushInterval {
				t.Fatalf("pushes %v apart, minimum is %v", h.now.Sub(lastPush), syncPushInterval)
			}
			lastPush = h.now
			h.svc.process(reader.ackPacket(plain[:9+len(received[len(received)-1])], floodVia()), h.now)
		}
		h.advance(50 * time.Millisecond)
		if h.now.Sub(testEpoch) > time.Minute {
			t.Fatalf("only received %v", received)
		}
	}
	if strings.Join(received, ",") != "first,second,third" {
		t.Fatalf("order %v", received)
	}
	c := h.clientFor(reader)
	posts, _ := h.repo.LatestPosts(testContext(), "lobby", 10)
	if c.syncSince != posts[2].Timestamp || h.svc.Stats().PushesAcked != 3 {
		t.Fatalf("cursor %d, last post %d, stats %+v", c.syncSince, posts[2].Timestamp, h.svc.Stats())
	}

	// Nothing is owed any more.
	h.advance(5 * time.Second)
	h.tx.none(t)
}

func TestOnePushInFlightPerClientUntilAckOrTimeout(t *testing.T) {
	h := newHarness(t, nil)
	author := h.loggedIn(1, "guest-pass")
	h.loggedIn(2, "guest-pass")
	for _, text := range []string{"a", "b"} {
		pkt, _ := author.text(text, floodVia())
		h.svc.process(pkt, h.now)
		h.tx.only(t)
	}
	h.advance(9 * time.Second)
	count := len(h.tx.take())
	for i := 0; i < 100; i++ { // 10 s of ticks without an ACK, inside the 12 s timeout
		h.advance(100 * time.Millisecond)
		count += len(h.tx.take())
	}
	if count != 1 {
		t.Fatalf("%d pushes were sent before the first was acknowledged or timed out", count)
	}
}

func TestPushTimeoutsStopPushingAfterThreeFailuresUntilTheClientIsActive(t *testing.T) {
	h := newHarness(t, nil)
	author := h.loggedIn(1, "guest-pass")
	reader := h.loggedIn(2, "guest-pass")
	for _, text := range []string{"a", "b", "c", "d", "e"} {
		pkt, _ := author.text(text, floodVia())
		h.svc.process(pkt, h.now)
		h.tx.only(t)
	}
	c := h.clientFor(reader)

	pushes := 0
	for i := 0; i < 600; i++ { // 60 s
		h.advance(100 * time.Millisecond)
		pushes += len(h.tx.take())
	}
	if pushes != 3 || c.failures != 3 || h.svc.Stats().PushTimeouts != 3 {
		t.Fatalf("pushes=%d failures=%d stats=%+v; the engine must give up after three timeouts", pushes, c.failures, h.svc.Stats())
	}

	// Authenticated activity from the client resumes pushing.
	ka, _ := reader.keepAlive(0, floodVia())
	h.svc.process(ka, h.now)
	if c.failures != 0 {
		t.Fatalf("a request must reset the failure count: %d", c.failures)
	}
	resumed := 0
	for i := 0; i < 100; i++ {
		h.advance(100 * time.Millisecond)
		resumed += len(h.tx.take())
	}
	if resumed == 0 {
		t.Fatal("pushing did not resume after the client was active again")
	}
}

func TestThreeFailuresDropTheStoredRoute(t *testing.T) {
	h := newHarness(t, nil)
	author := h.loggedIn(1, "guest-pass")
	reader := h.loggedIn(2, "guest-pass")
	h.svc.process(reader.pathPacket([]byte{0xAA, 0xBB}, 1, mesh.PayloadAck, []byte{0, 0, 0, 0}, direct()), h.now)
	c := h.clientFor(reader)
	for _, text := range []string{"a", "b", "c", "d"} {
		pkt, _ := author.text(text, floodVia())
		h.svc.process(pkt, h.now)
		h.tx.only(t)
	}

	var first sentPacket
	for i := 0; i < 200 && first.Req.Raw == nil; i++ {
		h.advance(100 * time.Millisecond)
		if got := h.tx.take(); len(got) > 0 {
			first = got[0]
		}
	}
	if first.Req.Raw == nil || !first.Pkt.IsDirect() || !bytes.Equal(first.Pkt.Path, []byte{0xAA, 0xBB}) {
		t.Fatalf("a known route must be used for the push: %+v", first)
	}
	if want := pushTimeoutBase + pushTimeoutFactor*3; c.ackDeadline.Sub(h.now) > want || c.ackDeadline.Sub(h.now) < want-time.Second {
		t.Fatalf("direct timeout should be 4 s + 2 s x (hops+1) = %v, got %v", want, c.ackDeadline.Sub(h.now))
	}
	for i := 0; i < 700 && c.outPathKnown(); i++ {
		h.advance(100 * time.Millisecond)
		h.tx.take()
	}
	if c.outPathKnown() || c.failures != 3 {
		t.Fatalf("route=%v failures=%d; three failures must drop the route", c.outPathKnown(), c.failures)
	}
}

func TestPushStillQueuedAtTimeoutIsWithdrawnWithoutBlame(t *testing.T) {
	h := newHarness(t, nil)
	author := h.loggedIn(1, "guest-pass")
	reader := h.loggedIn(2, "guest-pass")
	pkt, _ := author.text("a", floodVia())
	h.svc.process(pkt, h.now)
	h.tx.only(t)
	c := h.clientFor(reader)
	tag := h.room.pushTag(c)

	for i := 0; i < 200 && !c.hasPending; i++ {
		h.advance(100 * time.Millisecond)
	}
	if !c.hasPending {
		t.Fatal("no push was attempted")
	}
	h.tx.take()
	h.tx.queued[tag] = 1 // the scheduler is still holding it back (duty cycle)
	h.advance(13 * time.Second)
	if c.hasPending || c.failures != 0 || h.svc.Stats().PushTimeouts != 0 {
		t.Fatalf("pending=%v failures=%d: a push that never left the queue is not a failed push", c.hasPending, c.failures)
	}
}

func TestAckArrivesViaPathExtraAndMultipart(t *testing.T) {
	for name, deliver := range map[string]func(p *peer, h *harness, plain []byte){
		"PATH extra": func(p *peer, h *harness, plain []byte) {
			ack := mesh.AckHash(plain, p.pubSlice())
			h.svc.process(p.pathPacket([]byte{0xAA}, 1, mesh.PayloadAck, ack[:], direct()), h.now)
		},
		"multipart": func(p *peer, h *harness, plain []byte) {
			ack := mesh.AckHash(plain, p.pubSlice())
			h.svc.process(direct().packet(mesh.PayloadMultipart, append([]byte{0x03}, ack[:]...)), h.now)
		},
		"plain ACK": func(p *peer, h *harness, plain []byte) { h.svc.process(p.ackPacket(plain, floodVia()), h.now) },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			author := h.loggedIn(1, "guest-pass")
			reader := h.loggedIn(2, "guest-pass")
			pkt, _ := author.text("x", floodVia())
			h.svc.process(pkt, h.now)
			h.tx.only(t)
			var plain []byte
			for i := 0; i < 200 && plain == nil; i++ {
				h.advance(100 * time.Millisecond)
				for _, sp := range h.tx.take() {
					plain = reader.open(sp)[:9+1]
				}
			}
			if plain == nil {
				t.Fatal("no push")
			}
			c := h.clientFor(reader)
			deliver(reader, h, plain)
			if c.hasPending || c.syncSince == 0 || h.svc.Stats().PushesAcked != 1 {
				t.Fatalf("ACK not honoured: pending=%v since=%d", c.hasPending, c.syncSince)
			}
		})
	}
}

func TestAnAckForAnotherClientsPushIsIgnored(t *testing.T) {
	h := newHarness(t, nil)
	reader := h.loggedIn(1, "guest-pass")
	c := h.clientFor(reader)
	c.hasPending, c.pendingAck, c.pushedPost = true, [4]byte{9, 9, 9, 9}, 5
	h.svc.process(reader.ackPacket([]byte("unrelated"), floodVia()), h.now)
	if !c.hasPending || c.syncSince != 0 {
		t.Fatal("an unrelated ACK advanced the cursor")
	}
	h.svc.process(floodVia().packet(mesh.PayloadAck, []byte{1, 2, 3}), h.now) // too short
	h.svc.process(floodVia().packet(mesh.PayloadMultipart, []byte{0x13, 1, 2, 3}), h.now)
	h.svc.process(floodVia().packet(mesh.PayloadMultipart, []byte{0x12, 9, 9, 9, 9}), h.now) // inner type is not ACK
	if !c.hasPending {
		t.Fatal("a malformed ACK variant was accepted")
	}
}

func TestLoginSyncSinceAndBacklogWindow(t *testing.T) {
	// Ten posts already exist; the room keeps the newest three for push.
	cfg := testRoomConfig()
	cfg.BacklogPosts = 3
	h := newHarnessWith(t, harnessOptions{rooms: []models.RoomConfig{cfg}})
	base := uint32(h.now.Unix()) - 1000
	for i := uint32(0); i < 10; i++ {
		post := db.RoomPost{Timestamp: base + i, Author: bytes.Repeat([]byte{0xEE}, 32), Text: "old " + string(rune('0'+i))}
		if err := h.repo.AppendPost(testContext(), "lobby", post, nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	h = newHarnessWith(t, harnessOptions{rooms: []models.RoomConfig{cfg}, repo: h.repo})
	if len(h.room.posts) != 3 || h.room.posts[0].Timestamp != base+7 {
		t.Fatalf("cache: %+v", h.room.posts)
	}

	collect := func(p *peer, since uint32) []string {
		h.login(p, "guest-pass", since, floodVia())
		var got []string
		for i := 0; i < 400; i++ {
			h.advance(100 * time.Millisecond)
			for _, sp := range h.tx.take() {
				plain := p.open(sp)
				text := string(bytes.TrimRight(plain[9:], "\x00"))
				got = append(got, text)
				h.svc.process(p.ackPacket(plain[:9+len(text)], floodVia()), h.now)
			}
		}
		return got
	}
	if got := collect(h.newPeer(1), 0); strings.Join(got, ",") != "old 7,old 8,old 9" {
		t.Fatalf("a new client gets the newest three only: %v", got)
	}
	if got := collect(h.newPeer(2), base+8); strings.Join(got, ",") != "old 9" {
		t.Fatalf("sync_since must be honoured: %v", got)
	}
	if got := collect(h.newPeer(3), base+9); len(got) != 0 {
		t.Fatalf("a client that is up to date got %v", got)
	}
}

func TestKeepAliveOnADirectRoute(t *testing.T) {
	h := newHarness(t, nil)
	author := h.loggedIn(1, "guest-pass")
	reader := h.loggedIn(2, "guest-pass")
	for _, text := range []string{"a", "b"} {
		pkt, _ := author.text(text, floodVia())
		h.svc.process(pkt, h.now)
		h.tx.only(t)
	}
	c := h.clientFor(reader)

	// No stored route: the keep-alive is processed but not answered.
	ka, _ := reader.keepAlive(0, direct())
	h.svc.process(ka, h.now)
	h.tx.none(t)

	h.svc.process(reader.pathPacket([]byte{0xAA}, 1, mesh.PayloadAck, []byte{0, 0, 0, 0}, direct()), h.now)
	c.hasPending = true
	ka, plain := reader.keepAlive(0, direct())
	h.svc.process(ka, h.now)
	reply := h.tx.only(t)
	if !reply.Pkt.IsDirect() || reply.Pkt.PayloadType() != mesh.PayloadAck || len(reply.Pkt.Payload) != 5 {
		t.Fatalf("keep-alive reply: %+v", reply.Pkt)
	}
	want := mesh.AckHash(plain[:9], reader.pubSlice())
	if !bytes.Equal(reply.Pkt.Payload[:4], want[:]) || reply.Pkt.Payload[4] != 2 {
		t.Fatalf("reply %x, want ACK %x and an unsynced count of 2", reply.Pkt.Payload, want)
	}
	if reply.Req.Delay != serverResponseDelay || c.hasPending {
		t.Fatalf("delay %v, pending %v", reply.Req.Delay, c.hasPending)
	}

	// forceSince moves the cursor, which is reflected in the count.
	posts, _ := h.repo.LatestPosts(testContext(), "lobby", 10)
	ka, _ = reader.keepAlive(posts[0].Timestamp, direct())
	h.svc.process(ka, h.now)
	reply = h.tx.only(t)
	if c.syncSince != posts[0].Timestamp || reply.Pkt.Payload[4] != 1 {
		t.Fatalf("since=%d count=%d", c.syncSince, reply.Pkt.Payload[4])
	}

	// Keep-alive is honoured on direct routes only.
	ka, _ = reader.keepAlive(1, floodVia())
	h.svc.process(ka, h.now)
	h.tx.none(t)
	if c.syncSince != posts[0].Timestamp {
		t.Fatal("a flooded keep-alive changed the cursor")
	}
}

func TestRequestReplayAndUnsupportedTypes(t *testing.T) {
	h := newHarness(t, nil)
	p := h.loggedIn(1, "admin-pass")
	for _, reqType := range []byte{0x01, 0x03, 0x05, 0x77} {
		req, _ := p.request(reqType, 0, floodVia())
		h.svc.process(req, h.now)
		h.tx.none(t)
	}
	if h.svc.Stats().UnsupportedRequests != 4 {
		t.Fatalf("stats: %+v", h.svc.Stats())
	}
	h.svc.process(p.pathPacket([]byte{0xAA}, 1, mesh.PayloadAck, []byte{0, 0, 0, 0}, direct()), h.now)

	// A request older than one already seen is a replay: no state change, no reply.
	c := h.clientFor(p)
	stale := p.peerPacket(mesh.PayloadReq, func() []byte {
		plain := make([]byte, 9)
		binary.LittleEndian.PutUint32(plain, c.lastTimestamp-1)
		plain[4] = 0x02
		binary.LittleEndian.PutUint32(plain[5:], 12345)
		return plain
	}(), direct())
	h.svc.process(stale, h.now)
	h.tx.none(t)
	if c.syncSince == 12345 {
		t.Fatal("a replayed request changed the cursor")
	}
}

func TestPathPacketsStoreTheRouteAndNeverAnswer(t *testing.T) {
	h := newHarness(t, nil)
	p := h.loggedIn(1, "guest-pass")
	c := h.clientFor(p)

	h.svc.process(p.pathPacket([]byte{1, 2, 3, 4}, 2, mesh.PayloadAck, []byte{0, 0, 0, 0}, direct()), h.now)
	h.tx.none(t) // no reciprocal path
	if c.outPathLen != 0x42 || !bytes.Equal(c.outPath, []byte{1, 2, 3, 4}) {
		t.Fatalf("route %#x %x", c.outPathLen, c.outPath)
	}
	// A zero-hop route is a valid route.
	h.svc.process(p.pathPacket(nil, 1, mesh.PayloadAck, []byte{0, 0, 0, 0}, direct()), h.now)
	if c.outPathLen != 0x00 || !c.outPathKnown() {
		t.Fatalf("zero-hop route: %#x", c.outPathLen)
	}

	// A PATH with a reserved hash size or an over-long path is rejected.
	for name, bad := range map[string][]byte{
		"reserved hash size": {0xC1, 1, 2, 3, 4, 0x03},
		"path too long":      append([]byte{0x7F}, make([]byte, 80)...),
	} {
		payload, err := mesh.PeerPayload(h.id.Hash(), p.id.Hash(), p.secret[:], bad)
		if err != nil {
			t.Fatal(err)
		}
		before := c.outPathLen
		h.svc.process(direct().packet(mesh.PayloadPath, payload), h.now)
		if c.outPathLen != before {
			t.Fatalf("%s: the stored route changed", name)
		}
	}

	// A PATH from an unknown source is ignored.
	stranger := h.newPeer(9)
	h.svc.process(stranger.pathPacket([]byte{9}, 1, mesh.PayloadAck, []byte{0, 0, 0, 0}, direct()), h.now)
	if len(h.room.clients) != 1 {
		t.Fatal("a stranger got a table entry")
	}
}
