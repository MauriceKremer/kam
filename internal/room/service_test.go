package room

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"kam.bytecoder.nl/internal/db"
	"kam.bytecoder.nl/internal/kiss"
	"kam.bytecoder.nl/internal/mesh"
	"kam.bytecoder.nl/internal/models"
)

func TestSessionsSurviveARestartButPushesWaitForActivity(t *testing.T) {
	h := newHarness(t, nil)
	author := h.loggedIn(1, "guest-pass")
	reader := h.loggedIn(2, "guest-pass")
	h.svc.process(reader.pathPacket([]byte{0xAA}, 1, mesh.PayloadAck, []byte{0, 0, 0, 0}, direct()), h.now)
	pkt, _ := author.text("while you were away", floodVia())
	h.svc.process(pkt, h.now)
	h.tx.only(t)
	lastHeardAt := h.room.byKey[reader.pub()].lastActivity
	h.svc.flushAll(h.now) // what Stop does

	h2 := newHarnessWith(t, harnessOptions{repo: h.repo})
	if len(h2.room.clients) != 2 || h2.svc.Stats().Clients != 2 {
		t.Fatalf("restored %d clients", len(h2.room.clients))
	}
	c := h2.room.byKey[reader.pub()]
	if c == nil || c.role != RoleReadWrite || c.outPathLen != 0x01 || !bytes.Equal(c.outPath, []byte{0xAA}) || c.syncSince != 0 || c.lastTimestamp == 0 {
		t.Fatalf("restored client: %+v", c)
	}
	if !c.lastActivity.Equal(lastHeardAt) {
		t.Fatalf("restored last-heard time = %v, want %v", c.lastActivity, lastHeardAt)
	}

	// No login is needed to be understood, but nothing is pushed to a silent client.
	for i := 0; i < 300; i++ {
		h2.advance(100 * time.Millisecond)
	}
	h2.tx.none(t)

	ka, _ := reader.keepAlive(0, direct())
	h2.svc.process(ka, h2.now)
	reply := h2.tx.only(t)
	if reply.Pkt.PayloadType() != mesh.PayloadAck || reply.Pkt.Payload[4] != 1 {
		t.Fatalf("keep-alive reply %+v", reply.Pkt)
	}
	var push sentPacket
	for i := 0; i < 100 && push.Req.Raw == nil; i++ {
		h2.advance(100 * time.Millisecond)
		if got := h2.tx.take(); len(got) > 0 {
			push = got[0]
		}
	}
	if push.Req.Raw == nil || !push.Pkt.IsDirect() || string(bytes.TrimRight(reader.open(push)[9:], "\x00")) != "while you were away" {
		t.Fatalf("pushing did not resume along the stored route: %+v", push)
	}

	// Posting works without logging in again, and replays stay blocked.
	pkt, _ = reader.text("back again", direct())
	h2.svc.process(pkt, h2.now)
	h2.tx.only(t)
	stale, _ := reader.textAt(1, mesh.TxtTypePlain, 0, "replayed from before the restart", direct())
	h2.svc.process(stale, h2.now)
	h2.tx.none(t)
	if h2.postCount() != 2 {
		t.Fatalf("posts: %d", h2.postCount())
	}
}

func TestMonitorSnapshotIsServedByTheRoomWorker(t *testing.T) {
	h := newHarness(t, nil)
	h.svc.Start()
	defer h.svc.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	snapshot, err := h.svc.MonitorSnapshot(ctx)
	if err != nil || len(snapshot) != 1 || snapshot[0].ID != "lobby" || snapshot[0].Clients == nil {
		t.Fatalf("snapshot = %+v, %v", snapshot, err)
	}
	snapshot[0].Name = "mutated"
	second, err := h.svc.MonitorSnapshot(ctx)
	if err != nil || second[0].Name != h.room.cfg.Name {
		t.Fatalf("snapshot exposed worker state: %+v, %v", second, err)
	}
}

func TestRoomActivityPublishesMonitorEvents(t *testing.T) {
	h := newHarness(t, nil)
	var events []models.MonitorEvent
	h.svc.SetMonitorRecorder(func(event models.MonitorEvent) { events = append(events, event) })
	client := h.loggedIn(1, "guest-pass")
	pkt, _ := client.text("monitor event post", floodVia())
	h.svc.process(pkt, h.now)
	if len(events) < 2 || events[0].Kind != "room" || events[0].Event != "login_ok" || events[0].Room == nil || *events[0].Room != "lobby" || events[1].Event != "post" {
		t.Fatalf("room monitor events = %+v", events)
	}
}

func TestSuccessfulPostPersistsUpdatedLastHeardTime(t *testing.T) {
	h := newHarness(t, nil)
	client := h.loggedIn(1, "guest-pass")
	h.svc.flushAll(h.now)
	h.now = h.now.Add(10 * time.Second)
	pkt, _ := client.text("heard while posting", floodVia())
	h.svc.process(pkt, h.now)
	want := h.room.byKey[client.pub()].lastActivity
	h.svc.flushAll(h.now)
	stored, err := h.repo.LoadClients(testContext(), "lobby")
	if err != nil {
		t.Fatal(err)
	}
	publicKey := client.pub()
	for _, record := range stored {
		if bytes.Equal(record.PublicKey, publicKey[:]) {
			if record.LastHeardAt == nil || !record.LastHeardAt.Equal(want) {
				t.Fatalf("persisted last heard = %v, want %v", record.LastHeardAt, want)
			}
			return
		}
	}
	t.Fatal("posted client's session was not persisted")
}

func TestClientStateIsFlushedPeriodicallyOnStopAndRetriedAfterFailure(t *testing.T) {
	var flaky *flakyStore
	h := newHarnessWith(t, harnessOptions{store: func(s Store) Store { flaky = &flakyStore{Store: s}; return flaky }})
	h.loggedIn(1, "guest-pass")

	h.advance(time.Second)
	if flaky.saved != 0 {
		t.Fatal("client state was written on every change instead of in batches")
	}
	flaky.saveErr = os.ErrDeadlineExceeded
	h.advance(clientFlushInterval)
	if !h.room.clients[0].dirty {
		t.Fatal("a failed save must leave the client dirty so it is retried")
	}
	flaky.saveErr = nil
	h.advance(clientFlushInterval)
	if flaky.saved != 1 || h.room.clients[0].dirty {
		t.Fatalf("saved=%d dirty=%v", flaky.saved, h.room.clients[0].dirty)
	}
	if stored, _ := h.repo.LoadClients(testContext(), "lobby"); len(stored) != 1 {
		t.Fatalf("stored clients: %d", len(stored))
	}

	// A change made just before shutdown is written by Stop.
	h.room.clients[0].syncSince = 4242
	h.room.clients[0].dirty = true
	h.svc.Stop()
	if stored, _ := h.repo.LoadClients(testContext(), "lobby"); len(stored) != 1 || stored[0].SyncSince != 4242 {
		t.Fatalf("Stop did not flush: %+v", stored)
	}
}

func TestRetentionPrunesOnTick(t *testing.T) {
	cfg := testRoomConfig()
	cfg.BacklogPosts, cfg.MaxPosts, cfg.MaxAgeDays = 3, 5, 10
	h := newHarnessWith(t, harnessOptions{rooms: []models.RoomConfig{cfg}})
	now := uint32(h.now.Unix())
	day := uint32(24 * 60 * 60)
	insert := func(ts uint32) {
		if err := h.repo.AppendPost(testContext(), "lobby", db.RoomPost{Timestamp: ts, Author: bytes.Repeat([]byte{1}, 32), Text: "x"}, nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	insert(now - 30*day) // too old
	insert(now - 20*day) // too old
	for i := uint32(0); i < 8; i++ {
		insert(now - 100 + i) // recent; only the newest five may stay
	}
	h.advance(0)
	if got := h.postCount(); got != 5 {
		t.Fatalf("%d posts after pruning, want 5", got)
	}
	posts, _ := h.repo.LatestPosts(testContext(), "lobby", 10)
	if posts[0].Timestamp != now-100+3 {
		t.Fatalf("oldest remaining post %d", posts[0].Timestamp)
	}

	// The next run is ten minutes away.
	insert(now - 99)
	h.advance(time.Minute)
	if got := h.postCount(); got != 6 {
		t.Fatalf("pruned again too soon: %d", got)
	}
	h.advance(pruneInterval)
	if got := h.postCount(); got != 5 {
		t.Fatalf("%d posts after the next run, want 5", got)
	}
}

func TestRoomsAreIndependent(t *testing.T) {
	a, b := testRoomConfig(), testRoomConfig()
	b.ID, b.Name, b.AdminPassword, b.GuestPassword = "annex", "Annex", "annex-admin", "annex-guest"
	h := newHarnessWith(t, harnessOptions{rooms: []models.RoomConfig{a, b}})
	roomA, roomB := h.svc.rooms[0], h.svc.rooms[1]
	if roomA.hash == roomB.hash {
		t.Skip("seeded identities happen to share a hash")
	}

	p := h.newPeer(1) // addressed to room A
	h.login(p, "guest-pass", 0, floodVia())
	if len(roomA.clients) != 1 || len(roomB.clients) != 0 {
		t.Fatalf("clients: %d/%d", len(roomA.clients), len(roomB.clients))
	}

	// The same person in room B needs room B's key and password.
	pubB := roomB.id.PublicKey()
	secretB, _ := p.id.SharedSecret(pubB[:])
	payload, _ := mesh.AnonPayload(roomB.hash, p.pubSlice(), secretB[:], loginPlain(p.tick(), 0, "annex-guest"))
	h.svc.process(floodVia().packet(mesh.PayloadAnonReq, payload), h.now)
	reply := h.tx.only(t)
	_, _, _, extra, err := mesh.ParsePathReturn(func() []byte {
		plain, ok := mesh.MACThenDecrypt(secretB[:], reply.Pkt.Payload[2:])
		if !ok || reply.Pkt.Payload[1] != roomB.hash {
			t.Fatal("room B's reply is not from room B")
		}
		return plain
	}())
	if err != nil || extra[7] != RoleReadWrite {
		t.Fatalf("room B login: %x %v", extra, err)
	}
	if len(roomB.clients) != 1 {
		t.Fatal("room B has no client")
	}

	// Room A's password does not open room B.
	q := h.newPeer(2)
	qSecret, _ := q.id.SharedSecret(pubB[:])
	payload, _ = mesh.AnonPayload(roomB.hash, q.pubSlice(), qSecret[:], loginPlain(q.tick(), 0, "guest-pass"))
	h.svc.process(floodVia().packet(mesh.PayloadAnonReq, payload), h.now)
	h.tx.none(t)

	// A post in room A is stored in room A only.
	pkt, _ := p.text("for room A", floodVia())
	h.svc.process(pkt, h.now)
	h.tx.only(t)
	if n, _ := h.repo.CountPosts(testContext(), "lobby"); n != 1 {
		t.Fatalf("room A posts: %d", n)
	}
	if n, _ := h.repo.CountPosts(testContext(), "annex"); n != 0 {
		t.Fatalf("room B posts: %d", n)
	}
}

func TestRoomsSharingAHashAreTriedInTurn(t *testing.T) {
	first, second := twoIdentitiesSharingAHash(t)
	a, b := testRoomConfig(), testRoomConfig()
	b.ID, b.Name, b.AdminPassword, b.GuestPassword = "annex", "Annex", "annex-admin", "annex-guest"
	h := newHarnessWith(t, harnessOptions{rooms: []models.RoomConfig{a, b}, ids: []*mesh.Identity{first, second}})
	roomB := h.svc.rooms[1]

	p := h.newPeer(7)
	pubB := roomB.id.PublicKey()
	secretB, _ := p.id.SharedSecret(pubB[:])
	payload, _ := mesh.AnonPayload(roomB.hash, p.pubSlice(), secretB[:], loginPlain(p.tick(), 0, "annex-guest"))
	h.svc.process(floodVia().packet(mesh.PayloadAnonReq, payload), h.now)
	h.tx.only(t)
	if len(h.svc.rooms[0].clients) != 0 || len(roomB.clients) != 1 {
		t.Fatalf("the login reached room %d clients / %d clients", len(h.svc.rooms[0].clients), len(roomB.clients))
	}
}

func TestNewServiceValidatesItsInputs(t *testing.T) {
	repo, err := db.OpenRoomRepository(filepath.Join(t.TempDir(), "kam.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	id := seededIdentity(t, 1)
	good := Options{Rooms: []Spec{{Config: testRoomConfig(), Identity: id}}, Mesh: models.DefaultMeshConfig(), Store: repo, Transmitter: newFakeTx()}

	if _, err := NewService(good); err != nil {
		t.Fatalf("valid options rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Options){
		"no rooms":         func(o *Options) { o.Rooms = nil },
		"no store":         func(o *Options) { o.Store = nil },
		"no transmitter":   func(o *Options) { o.Transmitter = nil },
		"invalid mesh":     func(o *Options) { o.Mesh.PathHashSize = 7 },
		"missing identity": func(o *Options) { o.Rooms = []Spec{{Config: testRoomConfig()}} },
	} {
		opts := good
		mutate(&opts)
		if _, err := NewService(opts); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestChangedIdentityIsRecordedAndDoesNotStopTheRoom(t *testing.T) {
	h := newHarness(t, nil)
	replacement := seededIdentity(t, 99)
	h2 := newHarnessWith(t, harnessOptions{repo: h.repo, ids: []*mesh.Identity{replacement}})
	if h2.room.pub != replacement.PublicKey() {
		t.Fatal("room did not start with the new identity")
	}
	replacementKey := replacement.PublicKey()
	if previous, err := h.repo.RegisterRoom(testContext(), "lobby", replacementKey[:]); err != nil || previous != nil {
		t.Fatalf("the new key was not recorded: %x %v", previous, err)
	}
}

func TestHandlePacketDropsWhenTheQueueIsFull(t *testing.T) {
	h := newHarness(t, nil)
	pkt := floodVia().packet(mesh.PayloadAck, []byte{1, 2, 3, 4})
	for i := 0; i < inboundQueue+6; i++ {
		h.svc.HandlePacket(kiss.RxPacket{Packet: pkt})
	}
	if got := h.svc.Stats().PacketsDropped; got != 6 {
		t.Fatalf("dropped %d, want 6", got)
	}
}

func TestWorkerProcessesPacketsAndStopFlushes(t *testing.T) {
	h := newHarness(t, nil)
	sent := make(chan sentPacket, 8)
	h.tx.onSubmit = func(sp sentPacket) { sent <- sp }
	h.svc.Start()
	h.svc.Start() // idempotent

	p := h.newPeer(1)
	h.svc.HandlePacket(kiss.RxPacket{Packet: p.loginPacket("guest-pass", 0, floodVia()), ReceivedAt: time.Now()})
	select {
	case sp := <-sent:
		if sp.Pkt.PayloadType() != mesh.PayloadPath {
			t.Fatalf("reply type %d", sp.Pkt.PayloadType())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the worker never answered")
	}

	h.svc.Stop()
	h.svc.Stop() // idempotent
	if stored, _ := h.repo.LoadClients(testContext(), "lobby"); len(stored) != 1 {
		t.Fatalf("the session was not flushed on stop: %d", len(stored))
	}
	if h.svc.Stats().LoginsOK != 1 {
		t.Fatalf("stats: %+v", h.svc.Stats())
	}
}

func TestWorkerSendsStartupAdvertAndStopsPromptly(t *testing.T) {
	cfg := testRoomConfig()
	cfg.AdvertIntervalMin, cfg.FloodAdvertIntervalHours = 2, 47
	h := newHarnessWith(t, harnessOptions{rooms: []models.RoomConfig{cfg}})
	sent := make(chan sentPacket, 8)
	h.tx.onSubmit = func(sp sentPacket) { sent <- sp }
	h.svc.Start()
	select {
	case sp := <-sent:
		if sp.Pkt.PayloadType() != mesh.PayloadAdvert {
			t.Fatalf("first transmission is type %d", sp.Pkt.PayloadType())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no startup advert")
	}
	done := make(chan struct{})
	go func() { h.svc.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return")
	}
}

func TestStopWithoutStartStillFlushes(t *testing.T) {
	h := newHarness(t, nil)
	h.loggedIn(1, "guest-pass")
	h.svc.Stop()
	if stored, _ := h.repo.LoadClients(testContext(), "lobby"); len(stored) != 1 {
		t.Fatalf("stored clients: %d", len(stored))
	}
}

// --- adverts ---

func advertConfig() models.RoomConfig {
	cfg := testRoomConfig()
	cfg.AdvertIntervalMin, cfg.FloodAdvertIntervalHours = 2, 47
	return cfg
}

func checkAdvert(t *testing.T, sp sentPacket, h *harness, roomIndex int) uint32 {
	t.Helper()
	if sp.Pkt.PayloadType() != mesh.PayloadAdvert || sp.Req.Priority != kiss.TxPriorityAdvert {
		t.Fatalf("not an advert: %+v", sp)
	}
	pub := h.svc.rooms[roomIndex].pub
	payload := sp.Pkt.Payload
	if !bytes.Equal(payload[:32], pub[:]) {
		t.Fatalf("advert from %x, want %x", payload[:32], pub)
	}
	signed := append(append([]byte(nil), payload[:36]...), payload[100:]...)
	if !ed25519.Verify(pub[:], signed, payload[36:100]) {
		t.Fatal("advert signature does not verify")
	}
	appData := payload[100:]
	name := h.svc.rooms[roomIndex].cfg.Name
	if appData[0] != 0x83 || string(appData[1:]) != name {
		t.Fatalf("appdata %x, want type 3 with name %q", appData, name)
	}
	return binary.LittleEndian.Uint32(payload[32:36])
}

func TestAdvertCadence(t *testing.T) {
	meshCfg := models.DefaultMeshConfig()
	meshCfg.DefaultScope = "#eu"
	h := newHarnessWith(t, harnessOptions{rooms: []models.RoomConfig{advertConfig()}, mesh: &meshCfg})

	// Startup: one scoped flood advert.
	h.advance(0)
	first := h.tx.only(t)
	ts := []uint32{checkAdvert(t, first, h, 0)}
	scope := mesh.RegionKey("#eu")
	if first.Pkt.RouteType() != mesh.RouteTransportFlood || first.Pkt.TransportCodes[0] != scope.Code(mesh.PayloadAdvert, first.Pkt.Payload) || first.Pkt.PathLen != 0 {
		t.Fatalf("startup advert should be a flood in the default scope: %+v", first.Pkt)
	}
	if first.Req.Tag != "advert:lobby" || h.tx.cancels["advert:lobby"] != 1 {
		t.Fatalf("a stale queued advert must be replaced: tag=%q cancels=%v", first.Req.Tag, h.tx.cancels)
	}

	h.advance(time.Minute + 59*time.Second)
	h.tx.none(t)
	h.advance(time.Second) // two minutes after startup
	local := h.tx.only(t)
	ts = append(ts, checkAdvert(t, local, h, 0))
	if local.Pkt.RouteType() != mesh.RouteDirect || local.Pkt.PathLen != 0 || len(local.Pkt.Path) != 0 {
		t.Fatalf("the periodic advert is zero-hop: %+v", local.Pkt)
	}
	h.advance(2 * time.Minute)
	ts = append(ts, checkAdvert(t, h.tx.only(t), h, 0))

	// The flood advert comes after 47 hours and restarts the zero-hop timer.
	h.advance(47*time.Hour - 4*time.Minute)
	flood := h.tx.only(t)
	if flood.Pkt.RouteType() != mesh.RouteTransportFlood {
		t.Fatalf("after 47 h: %+v", flood.Pkt)
	}
	ts = append(ts, checkAdvert(t, flood, h, 0))
	h.advance(time.Minute)
	h.tx.none(t)
	h.advance(time.Minute)
	ts = append(ts, checkAdvert(t, h.tx.only(t), h, 0))

	for i := 1; i < len(ts); i++ {
		if ts[i] <= ts[i-1] {
			t.Fatalf("advert timestamps must strictly increase: %v", ts)
		}
	}
}

func TestAdvertsCanBeSwitchedOffIndependently(t *testing.T) {
	cases := []struct {
		name                string
		localMin, floodHour int
		wantFirst           func(mesh.Packet) bool
	}{
		{"flood only", 0, 3, func(p mesh.Packet) bool { return p.IsFlood() }},
		{"zero-hop only", 2, 0, func(p mesh.Packet) bool { return p.IsDirect() }},
		{"neither", 0, 0, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testRoomConfig()
			cfg.AdvertIntervalMin, cfg.FloodAdvertIntervalHours = tc.localMin, tc.floodHour
			h := newHarnessWith(t, harnessOptions{rooms: []models.RoomConfig{cfg}})
			h.advance(0)
			if tc.wantFirst == nil {
				h.advance(100 * time.Hour)
				h.tx.none(t)
				return
			}
			first := h.tx.only(t)
			if !tc.wantFirst(first.Pkt) {
				t.Fatalf("startup advert has the wrong kind: %+v", first.Pkt)
			}
			h.advance(4 * time.Hour)
			if got := h.tx.take(); len(got) == 0 || !tc.wantFirst(got[0].Pkt) {
				t.Fatalf("periodic adverts of the other kind are not wanted: %+v", got)
			}
		})
	}
}

func TestStartupAdvertsAreStaggeredAcrossRooms(t *testing.T) {
	a, b := advertConfig(), advertConfig()
	b.ID, b.Name = "annex", "Annex"
	h := newHarnessWith(t, harnessOptions{rooms: []models.RoomConfig{a, b}})
	h.advance(0)
	checkAdvert(t, h.tx.only(t), h, 0)
	h.advance(time.Second)
	h.tx.none(t)
	h.advance(time.Second)
	checkAdvert(t, h.tx.only(t), h, 1)
}

func TestAdvertCarriesLocationAndUsesConfiguredHashSize(t *testing.T) {
	cfg := advertConfig()
	cfg.Name = "Lobby"
	cfg.Location = &models.LocationConfig{Latitude: 52.25, Longitude: -4.5}
	meshCfg := models.DefaultMeshConfig()
	meshCfg.PathHashSize = 2
	id := seededIdentity(t, 5)
	pkt, err := BuildAdvert(id, cfg, meshCfg, 1234, true)
	if err != nil {
		t.Fatal(err)
	}
	appData := pkt.Payload[100:]
	if appData[0] != 0x93 || int32(binary.LittleEndian.Uint32(appData[1:5])) != 52_250_000 || int32(binary.LittleEndian.Uint32(appData[5:9])) != -4_500_000 {
		t.Fatalf("appdata %x", appData)
	}
	if pkt.HashSize() != 2 || pkt.HashCount() != 0 || pkt.RouteType() != mesh.RouteFlood {
		t.Fatalf("flood advert framing: %+v", pkt)
	}
	zero, err := BuildAdvert(id, cfg, meshCfg, 1234, false)
	if err != nil || zero.RouteType() != mesh.RouteDirect || zero.PathLen != 0 {
		t.Fatalf("zero-hop advert: %+v %v", zero, err)
	}

	cfg.Name = "A name that is far too long to fit"
	if _, err := BuildAdvert(id, cfg, meshCfg, 1, true); err == nil {
		t.Fatal("an oversized advert name was accepted")
	}
}

// --- identities ---

type seedReader struct{ data []byte }

func (r *seedReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestLoadIdentitiesCreatesStoresAndReusesKeys(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rooms")
	rooms := []models.RoomConfig{{ID: "lobby"}, {ID: "annex"}, {ID: "garage"}}
	first, err := LoadIdentities(dir, rooms, seededRand(1))
	if err != nil {
		t.Fatal(err)
	}
	hashes := map[byte]bool{}
	for i, loaded := range first {
		if !loaded.Created {
			t.Fatalf("room %d was not created", i)
		}
		hashes[loaded.Identity.Hash()] = true
	}
	if len(hashes) != 3 {
		t.Fatal("rooms share a hash")
	}
	again, err := LoadIdentities(dir, rooms, seededRand(99))
	if err != nil {
		t.Fatal(err)
	}
	for i := range again {
		if again[i].Created || again[i].Identity.PublicKey() != first[i].Identity.PublicKey() {
			t.Fatalf("room %d changed identity across runs", i)
		}
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(IdentityPath(dir, "lobby"))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("key file: %v %v", info, err)
		}
	}
	if KeyDir("/data/kam.db") != "/data/rooms" {
		t.Fatalf("KeyDir = %q", KeyDir("/data/kam.db"))
	}
}

func TestLoadIdentitiesAvoidsAHashAnotherRoomUses(t *testing.T) {
	a, b := twoIdentitiesSharingAHash(t)
	var other *mesh.Identity
	for seed := byte(1); other == nil; seed++ {
		if c := seededIdentity(t, seed); c.Hash() != a.Hash() && mesh.ValidHash(c.Hash()) {
			other = c
		}
	}
	reader := &seedReader{data: append(append(a.Seed(), b.Seed()...), other.Seed()...)}
	loaded, err := LoadIdentities(t.TempDir(), []models.RoomConfig{{ID: "one"}, {ID: "two"}}, reader)
	if err != nil {
		t.Fatal(err)
	}
	if loaded[0].Identity.PublicKey() != a.PublicKey() {
		t.Fatal("first room should take the first key")
	}
	if loaded[1].Identity.Hash() == a.Hash() || loaded[1].Identity.PublicKey() != other.PublicKey() {
		t.Fatal("the second room reused a hash that the first room already answers to")
	}
}

func TestLoadIdentitiesRefusesACorruptKeyFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(IdentityPath(dir, "lobby"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadIdentities(dir, []models.RoomConfig{{ID: "lobby"}}, seededRand(1)); err == nil {
		t.Fatal("a corrupt key was replaced silently")
	}
}

func TestLoginLimiterBoundsItsMemory(t *testing.T) {
	l := newLoginLimiter()
	now := testEpoch
	for i := 0; i < maxTrackedKeys*3; i++ {
		var key [32]byte
		binary.LittleEndian.PutUint32(key[:], uint32(i))
		l.allow(key, now)
		now = now.Add(time.Millisecond)
	}
	if len(l.perKey) > maxTrackedKeys {
		t.Fatalf("tracking %d keys, bound is %d", len(l.perKey), maxTrackedKeys)
	}
	// Idle entries are reclaimed.
	now = now.Add(2 * time.Minute)
	var key [32]byte
	key[0] = 0xFF
	l.allow(key, now)
	if len(l.perKey) > maxTrackedKeys {
		t.Fatalf("tracking %d keys after idling", len(l.perKey))
	}
}

func TestRefusedTransmissionsAreCountedAndPushesStayOwed(t *testing.T) {
	h := newHarness(t, nil)
	author := h.loggedIn(1, "guest-pass")
	reader := h.loggedIn(2, "guest-pass")
	pkt, _ := author.text("owed", floodVia())
	h.svc.process(pkt, h.now)
	h.tx.only(t)

	h.tx.fail = kiss.ErrTxQueueFull
	var refused bool
	for i := 0; i < 200 && !refused; i++ {
		h.advance(100 * time.Millisecond)
		refused = h.svc.Stats().TxRejected > 0
	}
	if !refused {
		t.Fatal("the refused push was not counted")
	}
	if c := h.clientFor(reader); c.hasPending || c.failures != 0 {
		t.Fatalf("a push the queue refused is not in flight and not a failure: %+v", c)
	}

	// Once the queue accepts packets again the post is delivered.
	h.tx.fail = nil
	var delivered bool
	for i := 0; i < 100 && !delivered; i++ {
		h.advance(100 * time.Millisecond)
		delivered = len(h.tx.take()) > 0
	}
	if !delivered {
		t.Fatal("the post was lost after a refused push")
	}
}
