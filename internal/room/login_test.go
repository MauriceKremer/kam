package room

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"log/slog"
	"strings"
	"testing"
	"time"

	"kam.bytecoder.nl/internal/db"
	"kam.bytecoder.nl/internal/kiss"
	"kam.bytecoder.nl/internal/mesh"
	"kam.bytecoder.nl/internal/models"
)

func TestLoginRolesAndResponseLayout(t *testing.T) {
	cases := []struct {
		name     string
		password string
		mutate   func(*models.RoomConfig)
		role     uint8
		flag     byte // response[6]: 1 admin, 2 guest, 0 otherwise
		accepted bool
	}{
		{"admin password", "admin-pass", nil, RoleAdmin, 1, true},
		{"guest password gets read-write", "guest-pass", nil, RoleReadWrite, 0, true},
		{"wrong password is refused", "nope", nil, 0, 0, false},
		{"blank password is refused", "", nil, 0, 0, false},
		{"wrong password with allow_read_only is a guest", "nope", func(c *models.RoomConfig) { c.AllowReadOnly = true }, RoleGuest, 2, true},
		{"blank guest password without open_room is refused", "", func(c *models.RoomConfig) { c.GuestPassword = "" }, 0, 0, false},
		{"open room accepts blank as read-write", "", func(c *models.RoomConfig) { c.GuestPassword, c.OpenRoom = "", true }, RoleReadWrite, 0, true},
		{"open room still rejects a wrong password", "nope", func(c *models.RoomConfig) { c.GuestPassword, c.OpenRoom = "", true }, 0, 0, false},
		{"admin password wins over open room", "admin-pass", func(c *models.RoomConfig) { c.GuestPassword, c.OpenRoom = "", true }, RoleAdmin, 1, true},
		{"15 byte password", "123456789012345", func(c *models.RoomConfig) { c.AdminPassword = "123456789012345" }, RoleAdmin, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.mutate)
			p := h.newPeer(1)
			h.svc.process(p.loginPacket(tc.password, 0, floodVia(0xA1)), h.now)
			if !tc.accepted {
				h.tx.none(t)
				if len(h.room.clients) != 0 {
					t.Fatal("a refused login created a client")
				}
				if h.svc.Stats().LoginsFailed != 1 {
					t.Fatalf("stats: %+v", h.svc.Stats())
				}
				return
			}
			response := func() []byte {
				_, _, _, extra, err := mesh.ParsePathReturn(p.open(h.tx.only(t)))
				if err != nil {
					t.Fatal(err)
				}
				return extra[:13]
			}()
			if response[4] != 0 || response[5] != 0 || response[6] != tc.flag || response[7] != tc.role || response[12] != 1 {
				t.Fatalf("response %x: want flag=%d role=%d level=1", response, tc.flag, tc.role)
			}
			if got := binary.LittleEndian.Uint32(response[0:4]); got != uint32(h.now.Unix()) {
				t.Fatalf("server timestamp %d, want %d", got, h.now.Unix())
			}
			if h.clientFor(p).role != tc.role || h.svc.Stats().LoginsOK != 1 || h.svc.Stats().Clients != 1 {
				t.Fatalf("client state: %+v stats %+v", h.clientFor(p), h.svc.Stats())
			}
		})
	}
}

func TestLoginResponseCarriesRandomBlobSoRepliesDiffer(t *testing.T) {
	h := newHarness(t, nil)
	p := h.newPeer(1)
	first := h.login(p, "admin-pass", 0, floodVia())
	h.advance(time.Second)
	second := h.login(p, "admin-pass", 0, floodVia())
	if bytes.Equal(first[8:12], second[8:12]) || bytes.Equal(first[:4], second[:4]) {
		t.Fatalf("responses %x and %x would hash identically", first, second)
	}
}

func TestFloodLoginIsAnsweredWithAPathReturn(t *testing.T) {
	h := newHarness(t, nil)
	p := h.newPeer(1)
	h.svc.process(p.loginPacket("admin-pass", 0, route{flood: true, path: []byte{0xA1, 0xB2, 0xC3, 0xD4}, hashSize: 2}), h.now)
	sp := h.tx.only(t)

	if sp.Pkt.PayloadType() != mesh.PayloadPath || !sp.Pkt.IsFlood() {
		t.Fatalf("reply: type=%d flood=%v", sp.Pkt.PayloadType(), sp.Pkt.IsFlood())
	}
	if sp.Req.Priority != kiss.TxPriorityFloodReply || sp.Req.Delay != serverResponseDelay {
		t.Fatalf("priority=%d delay=%v", sp.Req.Priority, sp.Req.Delay)
	}
	if sp.Pkt.HashSize() != 2 || sp.Pkt.HashCount() != 0 {
		t.Fatalf("a reply mirrors the request's hash size: %+v", sp.Pkt)
	}
	pathLen, path, extraType, extra, err := mesh.ParsePathReturn(p.open(sp))
	if err != nil || pathLen != 0x42 || !bytes.Equal(path, []byte{0xA1, 0xB2, 0xC3, 0xD4}) || extraType != mesh.PayloadResponse {
		t.Fatalf("path return: len=%#x path=%x type=%d err=%v", pathLen, path, extraType, err)
	}
	if extra[4] != 0 || extra[7] != RoleAdmin {
		t.Fatalf("embedded response: %x", extra[:13])
	}
	if h.clientFor(p).outPathKnown() {
		t.Fatal("a flood login must clear the stored route")
	}
}

func TestDirectLoginIsAnsweredWithAResponseDatagram(t *testing.T) {
	h := newHarness(t, nil)
	p := h.newPeer(1)
	h.svc.process(p.loginPacket("admin-pass", 0, direct()), h.now)
	sp := h.tx.only(t)
	if sp.Pkt.PayloadType() != mesh.PayloadResponse || !sp.Pkt.IsFlood() || sp.Req.Priority != kiss.TxPriorityFloodReply {
		t.Fatalf("with no known route the response is flooded: %+v", sp)
	}

	// Once a route is known, a direct login gets a direct response.
	h.svc.process(p.pathPacket([]byte{0xAA, 0xBB}, 1, mesh.PayloadAck, []byte{9, 9, 9, 9}, direct()), h.now)
	h.advance(time.Second)
	h.svc.process(p.loginPacket("", 0, direct()), h.now)
	sp = h.tx.only(t)
	if sp.Pkt.PayloadType() != mesh.PayloadResponse || !sp.Pkt.IsDirect() || sp.Pkt.PathLen != 0x02 || !bytes.Equal(sp.Pkt.Path, []byte{0xAA, 0xBB}) || sp.Req.Priority != kiss.TxPriorityReply {
		t.Fatalf("expected a direct response along the stored route: %+v", sp)
	}
}

func TestReplyScopeFollowsTheRequest(t *testing.T) {
	meshCfg := models.DefaultMeshConfig()
	meshCfg.DefaultScope = "#home"
	meshCfg.Regions = []string{"#away"}
	home, away := mesh.RegionKey("#home"), mesh.RegionKey("#away")
	unknown := mesh.RegionKey("#elsewhere")

	cases := []struct {
		name  string
		route route
		want  *mesh.TransportKey
	}{
		{"request in a known region is answered in that region", route{flood: true, hashSize: 1, scope: &away}, &away},
		{"request in the default region", route{flood: true, hashSize: 1, scope: &home}, &home},
		{"an unscoped flood is answered unscoped", floodVia(), nil},
		{"a request whose code matches no region gets the default scope", route{flood: true, hashSize: 1, scope: &unknown}, &home},
		{"a direct request gets the default scope", direct(), &home},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarnessWith(t, harnessOptions{mesh: &meshCfg})
			p := h.newPeer(1)
			h.svc.process(p.loginPacket("admin-pass", 0, tc.route), h.now)
			sp := h.tx.only(t)
			if tc.want == nil {
				if sp.Pkt.RouteType() != mesh.RouteFlood {
					t.Fatalf("want an unscoped flood, got route %d", sp.Pkt.RouteType())
				}
				return
			}
			if sp.Pkt.RouteType() != mesh.RouteTransportFlood || sp.Pkt.TransportCodes[0] != tc.want.Code(sp.Pkt.PayloadType(), sp.Pkt.Payload) || sp.Pkt.TransportCodes[1] != 0 {
				t.Fatalf("scope mismatch: %+v", sp.Pkt)
			}
		})
	}

	t.Run("no regions configured sends unscoped", func(t *testing.T) {
		h := newHarness(t, nil)
		p := h.newPeer(1)
		h.svc.process(p.loginPacket("admin-pass", 0, direct()), h.now)
		if sp := h.tx.only(t); sp.Pkt.RouteType() != mesh.RouteFlood {
			t.Fatalf("route %d", sp.Pkt.RouteType())
		}
	})
}

func TestLoginReplayProtection(t *testing.T) {
	h := newHarness(t, nil)
	p := h.newPeer(1)
	h.svc.process(p.loginPacketAt(100, "admin-pass", 0, floodVia()), h.now)
	h.tx.only(t)

	for _, ts := range []uint32{100, 99, 1} {
		h.svc.process(p.loginPacketAt(ts, "admin-pass", 0, floodVia()), h.now)
		h.tx.none(t)
	}
	if got := h.svc.Stats().LoginsReplayed; got != 3 {
		t.Fatalf("replays counted: %d", got)
	}
	h.svc.process(p.loginPacketAt(101, "admin-pass", 0, floodVia()), h.now)
	h.tx.only(t)

	// A brand-new client may not log in with timestamp zero.
	q := h.newPeer(2)
	h.svc.process(q.loginPacketAt(0, "admin-pass", 0, floodVia()), h.now)
	h.tx.none(t)
}

func TestBlankPasswordResumesAKnownSessionWithoutResettingState(t *testing.T) {
	h := newHarness(t, nil)
	p := h.newPeer(1)
	h.login(p, "guest-pass", 77, floodVia(0xA1))
	c := h.clientFor(p)
	c.hasPending, c.failures, c.pushedPost = true, 2, 500
	c.ackDeadline = h.now.Add(time.Hour)
	c.outPathLen, c.outPath = 0x01, []byte{0xAA}
	lastTimestamp := c.lastTimestamp

	h.advance(time.Minute)
	h.svc.process(p.loginPacket("", 9999, direct()), h.now)
	sp := h.tx.only(t)
	if sp.Pkt.PayloadType() != mesh.PayloadResponse {
		t.Fatalf("resume reply type %d", sp.Pkt.PayloadType())
	}
	if c.syncSince != 77 || !c.hasPending || c.failures != 2 || c.outPathLen != 0x01 || c.lastTimestamp != lastTimestamp {
		t.Fatalf("a blank-password resume changed session state: %+v", c)
	}
	if c.role != RoleReadWrite {
		t.Fatalf("role changed to %d", c.role)
	}

	// An unknown sender with a blank password gets nothing.
	q := h.newPeer(2)
	h.svc.process(q.loginPacket("", 0, direct()), h.now)
	h.tx.none(t)
}

func TestPasswordLoginResetsPushState(t *testing.T) {
	h := newHarness(t, nil)
	p := h.newPeer(1)
	h.login(p, "guest-pass", 10, floodVia())
	c := h.clientFor(p)
	c.hasPending, c.failures = true, 3

	h.advance(time.Second)
	h.tx.cancels = map[string]int{}
	h.login(p, "guest-pass", 55, floodVia())
	if c.syncSince != 55 || c.hasPending || c.failures != 0 {
		t.Fatalf("login must reset cursor, pending ACK and failures: %+v", c)
	}
	if h.tx.cancels[h.room.pushTag(c)] == 0 {
		t.Fatal("a queued push for the old session was not cancelled")
	}
}

func TestLoginRateLimit(t *testing.T) {
	h := newHarness(t, nil)
	p := h.newPeer(1)
	for i := 0; i < 5; i++ {
		h.svc.process(p.loginPacket("wrong", 0, floodVia()), h.now)
	}
	if h.svc.Stats().LoginsFailed != 5 || h.svc.Stats().LoginsRateLimited != 0 {
		t.Fatalf("stats: %+v", h.svc.Stats())
	}
	// The sixth attempt from the same key is refused before any crypto work, so even
	// the correct password gets no answer.
	h.svc.process(p.loginPacket("admin-pass", 0, floodVia()), h.now)
	h.tx.none(t)
	if h.svc.Stats().LoginsRateLimited != 1 {
		t.Fatalf("stats: %+v", h.svc.Stats())
	}
	// Another key is unaffected, and the first key recovers as tokens refill.
	q := h.newPeer(2)
	h.svc.process(q.loginPacket("admin-pass", 0, floodVia()), h.now)
	h.tx.only(t)
	h.now = h.now.Add(2 * time.Second)
	h.svc.process(p.loginPacket("admin-pass", 0, floodVia()), h.now)
	h.tx.only(t)
}

func TestLoginRateLimitGlobalBudget(t *testing.T) {
	h := newHarness(t, nil)
	limited := 0
	for i := 0; i < 60; i++ {
		p := h.newPeer(byte(i + 1))
		before := h.svc.Stats().LoginsRateLimited
		h.svc.process(p.loginPacket("wrong", 0, floodVia()), h.now)
		if h.svc.Stats().LoginsRateLimited > before {
			limited++
		}
	}
	if limited < 30 {
		t.Fatalf("only %d of 60 simultaneous logins from distinct keys were limited", limited)
	}
}

func TestClientTableEvictsLeastRecentlyActiveNonAdmin(t *testing.T) {
	h := newHarness(t, func(c *models.RoomConfig) { c.MaxClients = 3 })
	admin, a, b, c := h.newPeer(1), h.newPeer(2), h.newPeer(3), h.newPeer(4)
	h.login(admin, "admin-pass", 0, floodVia())
	h.advance(time.Second)
	h.login(a, "guest-pass", 0, floodVia())
	h.advance(time.Second)
	h.login(b, "guest-pass", 0, floodVia())
	h.advance(time.Second)

	// a is the oldest non-admin; the admin is older still but exempt.
	h.login(c, "guest-pass", 0, floodVia())
	if h.room.byKey[a.pub()] != nil || h.room.byKey[admin.pub()] == nil || h.room.byKey[b.pub()] == nil || h.room.byKey[c.pub()] == nil {
		t.Fatalf("eviction picked the wrong client; table has %d entries", len(h.room.clients))
	}
	if len(h.room.clients) != 3 || h.svc.Stats().Clients != 3 {
		t.Fatalf("clients %d, gauge %d", len(h.room.clients), h.svc.Stats().Clients)
	}
	stored, _ := h.repo.LoadClients(testContext(), "lobby")
	for _, s := range stored {
		if bytes.Equal(s.PublicKey, a.pubSlice()) {
			t.Fatal("evicted client is still stored")
		}
	}
}

func TestClientTableFullOfAdminsRefusesLogin(t *testing.T) {
	h := newHarness(t, func(c *models.RoomConfig) { c.MaxClients = 2 })
	h.login(h.newPeer(1), "admin-pass", 0, floodVia())
	h.login(h.newPeer(2), "admin-pass", 0, floodVia())
	extra := h.newPeer(3)
	h.svc.process(extra.loginPacket("admin-pass", 0, floodVia()), h.now)
	h.tx.none(t)
	if len(h.room.clients) != 2 || h.svc.Stats().LoginsFailed != 1 {
		t.Fatalf("clients %d, stats %+v", len(h.room.clients), h.svc.Stats())
	}
}

func TestSharedSourceHashTriesEveryClient(t *testing.T) {
	h := newHarness(t, nil)
	first, second := twoIdentitiesSharingAHash(t)
	a, b := h.newPeerWith(first), h.newPeerWith(second)
	h.login(a, "guest-pass", 0, floodVia())
	h.login(b, "guest-pass", 0, floodVia())

	// Both clients share src_hash 0x21; b's post must still be matched to b.
	pkt, _ := b.text("from b", floodVia())
	h.svc.process(pkt, h.now)
	h.tx.only(t)
	posts, _ := h.repo.LatestPosts(testContext(), "lobby", 10)
	if len(posts) != 1 || !bytes.Equal(posts[0].Author, b.pubSlice()) {
		t.Fatalf("post attributed to the wrong client: %+v", posts)
	}
}

func TestDirectPacketsWithRemainingPathAreForwardersBusiness(t *testing.T) {
	h := newHarness(t, nil)
	p := h.newPeer(1)
	h.svc.process(p.loginPacket("admin-pass", 0, route{path: []byte{0x11}, hashSize: 1}), h.now)
	h.tx.none(t)
	if len(h.room.clients) != 0 {
		t.Fatal("a direct login with hops left was processed")
	}

	h.login(p, "admin-pass", 0, floodVia())
	pkt, _ := p.text("hello", route{path: []byte{0x11}, hashSize: 1})
	h.svc.process(pkt, h.now)
	h.tx.none(t)
	if h.postCount() != 0 {
		t.Fatal("a direct post with hops left was stored")
	}

	// An ACK is processed early even with hops left.
	q := h.newPeer(2)
	h.login(q, "guest-pass", 0, floodVia())
	c := h.clientFor(q)
	c.hasPending, c.pendingAck, c.pushedPost = true, [4]byte{1, 2, 3, 4}, 321
	h.svc.process(route{path: []byte{0x11}, hashSize: 1}.packet(mesh.PayloadAck, []byte{1, 2, 3, 4}), h.now)
	if c.hasPending || c.syncSince != 321 {
		t.Fatalf("early ACK ignored: %+v", c)
	}
}

func TestMalformedAndForeignPacketsAreDroppedSilently(t *testing.T) {
	h := newHarness(t, nil)
	p := h.newPeer(1)
	h.login(p, "admin-pass", 0, floodVia())

	valid, _ := p.text("hello", floodVia())
	var packets []mesh.Packet
	for cut := 0; cut < len(valid.Payload); cut++ {
		short := valid
		short.Payload = valid.Payload[:cut]
		if cut > 0 {
			packets = append(packets, short)
		}
	}
	for i := range valid.Payload {
		flipped := valid
		flipped.Payload = append([]byte(nil), valid.Payload...)
		flipped.Payload[i] ^= 0x5A
		packets = append(packets, flipped)
	}
	for _, kind := range []byte{mesh.PayloadReq, mesh.PayloadTxtMsg, mesh.PayloadPath, mesh.PayloadAnonReq, mesh.PayloadAck, mesh.PayloadMultipart, mesh.PayloadResponse, mesh.PayloadAdvert} {
		for _, size := range []int{1, 2, 3, 4, 5, 6, 17, 34, 35, 36, 37, 50, 100, 184} {
			payload := bytes.Repeat([]byte{0x40}, size)
			payload[0] = h.id.Hash()
			packets = append(packets, floodVia().packet(kind, payload))
		}
	}
	posts := h.postCount()
	for _, pkt := range packets {
		h.svc.process(pkt, h.now)
	}
	if h.postCount() != posts {
		t.Fatalf("garbage created posts: %d -> %d", posts, h.postCount())
	}
	h.tx.none(t)
}

func TestStoredClientWithBadKeyIsSkippedAtLoad(t *testing.T) {
	h := newHarness(t, nil)
	repo := h.repo
	good := h.newPeer(1)
	if err := repo.SaveClients(testContext(), "lobby", []db.RoomClient{
		{PublicKey: good.pubSlice(), Role: RoleReadWrite, OutPathLen: 0xFF},
		{PublicKey: []byte{1, 2, 3}, Role: RoleAdmin, OutPathLen: 0xFF},
		{PublicKey: append([]byte{1}, make([]byte, 31)...), Role: RoleAdmin, OutPathLen: 0xFF}, // identity point
	}); err != nil {
		t.Fatal(err)
	}
	reloaded := newHarnessWith(t, harnessOptions{repo: repo})
	if len(reloaded.room.clients) != 1 || reloaded.room.clients[0].pub != good.pub() {
		t.Fatalf("clients after load: %d", len(reloaded.room.clients))
	}
}

func TestLogsNeverContainPasswordsOrFullKeys(t *testing.T) {
	buf := &bytes.Buffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	h := newHarness(t, nil)
	p := h.newPeer(1)
	h.svc.process(p.loginPacket("hunter2-wrong", 0, floodVia()), h.now)
	h.login(p, "admin-pass", 0, floodVia())
	h.advance(time.Second)
	q := h.newPeer(2)
	h.login(q, "guest-pass", 0, floodVia())

	logged := buf.String()
	if !strings.Contains(logged, "login refused") || !strings.Contains(logged, "login accepted") {
		t.Fatalf("expected login log lines, got:\n%s", logged)
	}
	secrets := map[string]string{
		"wrong password": "hunter2-wrong", "admin password": "admin-pass", "guest password": "guest-pass",
		"client public key": hex.EncodeToString(p.pubSlice()), "room public key": hex.EncodeToString(h.room.pub[:]),
		"room private seed": hex.EncodeToString(h.id.Seed()),
	}
	for name, secret := range secrets {
		if strings.Contains(logged, secret) {
			t.Fatalf("log output leaks the %s:\n%s", name, logged)
		}
	}
}
