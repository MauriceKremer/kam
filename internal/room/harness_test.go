package room

import (
	"bytes"
	"context"
	"encoding/binary"
	"math/rand"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"kam.bytecoder.nl/internal/db"
	"kam.bytecoder.nl/internal/kiss"
	"kam.bytecoder.nl/internal/mesh"
	"kam.bytecoder.nl/internal/models"
)

var testEpoch = time.Unix(1_700_000_000, 0)

// sentPacket is a packet the service asked the radio to transmit.
type sentPacket struct {
	Req kiss.TxRequest
	Pkt mesh.Packet
}

// fakeTx records transmit requests and lets tests decide what Cancel reports.
type fakeTx struct {
	mu       sync.Mutex
	sent     []sentPacket
	queued   map[string]int // tag -> packets still "waiting in the queue"
	cancels  map[string]int
	fail     error
	onSubmit func(sentPacket)
}

func newFakeTx() *fakeTx {
	return &fakeTx{queued: map[string]int{}, cancels: map[string]int{}}
}

func (f *fakeTx) Submit(req kiss.TxRequest) error {
	pkt, err := mesh.Parse(req.Raw)
	if err != nil {
		return err
	}
	f.mu.Lock()
	if f.fail != nil {
		err := f.fail
		f.mu.Unlock()
		return err
	}
	sp := sentPacket{Req: req, Pkt: pkt}
	f.sent = append(f.sent, sp)
	hook := f.onSubmit
	f.mu.Unlock()
	if hook != nil {
		hook(sp)
	}
	return nil
}

func (f *fakeTx) Cancel(tag string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels[tag]++
	n := f.queued[tag]
	delete(f.queued, tag)
	return n
}

// take returns and clears everything sent so far.
func (f *fakeTx) take() []sentPacket {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.sent
	f.sent = nil
	return out
}

// only returns the single packet sent since the last take, failing the test
// otherwise.
func (f *fakeTx) only(t *testing.T) sentPacket {
	t.Helper()
	got := f.take()
	if len(got) != 1 {
		t.Fatalf("expected exactly one transmitted packet, got %d: %+v", len(got), got)
	}
	return got[0]
}

func (f *fakeTx) none(t *testing.T) {
	t.Helper()
	if got := f.take(); len(got) != 0 {
		t.Fatalf("expected no transmission, got %d packets: %+v", len(got), got)
	}
}

// flakyStore wraps a Store so tests can inject failures.
type flakyStore struct {
	Store
	appendErr error
	saveErr   error
	saved     int
}

func (f *flakyStore) AppendPost(ctx context.Context, roomID string, post db.RoomPost, key []byte, ts uint32) error {
	if f.appendErr != nil {
		return f.appendErr
	}
	return f.Store.AppendPost(ctx, roomID, post, key, ts)
}

func (f *flakyStore) SaveClients(ctx context.Context, roomID string, clients []db.RoomClient) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.saved++
	return f.Store.SaveClients(ctx, roomID, clients)
}

func testContext() context.Context { return context.Background() }

func seededRand(seed int64) *rand.Rand { return rand.New(rand.NewSource(seed)) }

func testRoomConfig() models.RoomConfig {
	room := models.DefaultRoomConfig()
	room.ID, room.Name = "lobby", "Lobby"
	room.AdminPassword, room.GuestPassword = "admin-pass", "guest-pass"
	// Adverts are exercised by their own tests; elsewhere they would add noise.
	room.AdvertIntervalMin, room.FloodAdvertIntervalHours = 0, 0
	return room
}

func seededIdentity(t testing.TB, seed byte) *mesh.Identity {
	t.Helper()
	id, err := mesh.NewIdentity(bytes.Repeat([]byte{seed}, mesh.SeedSize))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// identityWithHash finds a deterministic identity whose first public-key byte
// satisfies want.
func identityWithHash(t testing.TB, want func(byte) bool) *mesh.Identity {
	t.Helper()
	for seed := 1; seed < 20000; seed++ {
		var s [mesh.SeedSize]byte
		binary.LittleEndian.PutUint32(s[:], uint32(seed))
		id, err := mesh.NewIdentity(s[:])
		if err != nil {
			t.Fatal(err)
		}
		if mesh.ValidHash(id.Hash()) && want(id.Hash()) {
			return id
		}
	}
	t.Fatal("no identity with the wanted hash")
	return nil
}

// twoIdentitiesSharingAHash finds two deterministic identities with the same
// first public-key byte, which one-byte addressing cannot tell apart.
func twoIdentitiesSharingAHash(t testing.TB) (*mesh.Identity, *mesh.Identity) {
	t.Helper()
	seen := map[byte]*mesh.Identity{}
	for seed := 1; seed < 20000; seed++ {
		var s [mesh.SeedSize]byte
		binary.LittleEndian.PutUint32(s[:], uint32(seed))
		id, err := mesh.NewIdentity(s[:])
		if err != nil {
			t.Fatal(err)
		}
		if !mesh.ValidHash(id.Hash()) {
			continue
		}
		if other, ok := seen[id.Hash()]; ok {
			return other, id
		}
		seen[id.Hash()] = id
	}
	t.Fatal("no two identities share a hash")
	return nil, nil
}

type harness struct {
	t     *testing.T
	svc   *Service
	tx    *fakeTx
	repo  *db.RoomRepository
	store Store
	now   time.Time
	room  *roomState
	cfg   models.RoomConfig
	mesh  models.MeshConfig
	id    *mesh.Identity
}

type harnessOptions struct {
	rooms []models.RoomConfig // nil means one default room
	mesh  *models.MeshConfig
	repo  *db.RoomRepository // reuse a repository to simulate a restart
	store func(Store) Store
	ids   []*mesh.Identity
}

func newHarness(t *testing.T, mutate func(*models.RoomConfig)) *harness {
	t.Helper()
	cfg := testRoomConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	return newHarnessWith(t, harnessOptions{rooms: []models.RoomConfig{cfg}})
}

func newHarnessWith(t *testing.T, opts harnessOptions) *harness {
	t.Helper()
	repo := opts.repo
	if repo == nil {
		var err error
		repo, err = db.OpenRoomRepository(filepath.Join(t.TempDir(), "kam.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = repo.Close() })
	}
	var store Store = repo
	if opts.store != nil {
		store = opts.store(repo)
	}
	rooms := opts.rooms
	if len(rooms) == 0 {
		rooms = []models.RoomConfig{testRoomConfig()}
	}
	meshCfg := models.DefaultMeshConfig()
	if opts.mesh != nil {
		meshCfg = *opts.mesh
	}
	h := &harness{t: t, tx: newFakeTx(), repo: repo, store: store, now: testEpoch, cfg: rooms[0], mesh: meshCfg}
	var specs []Spec
	for i, cfg := range rooms {
		var id *mesh.Identity
		if i < len(opts.ids) {
			id = opts.ids[i]
		} else {
			id = seededIdentity(t, byte(0x40+i))
		}
		specs = append(specs, Spec{Config: cfg, Identity: id})
	}
	h.id = specs[0].Identity
	svc, err := NewService(Options{
		Rooms: specs, Mesh: meshCfg, Store: store, Transmitter: h.tx,
		Now: func() time.Time { return h.now }, Random: rand.New(rand.NewSource(1)),
	})
	if err != nil {
		t.Fatal(err)
	}
	h.svc, h.room = svc, svc.rooms[0]
	return h
}

// advance moves the clock and runs one tick.
func (h *harness) advance(d time.Duration) {
	h.now = h.now.Add(d)
	h.svc.tick(h.now)
}

// route describes how a test packet reaches the room.
type route struct {
	flood    bool
	path     []byte
	hashSize int
	scope    *mesh.TransportKey
}

func floodVia(hops ...byte) route { return route{flood: true, path: hops, hashSize: 1} }
func direct() route               { return route{} }

func (r route) packet(payloadType byte, payload []byte) mesh.Packet {
	hashSize := r.hashSize
	if hashSize == 0 {
		hashSize = 1
	}
	count := len(r.path) / hashSize
	pathLen, _ := mesh.EncodePathLen(hashSize, count)
	header := mesh.MakeHeader(mesh.RouteDirect, payloadType)
	if r.flood {
		header = mesh.MakeHeader(mesh.RouteFlood, payloadType)
	}
	p := mesh.Packet{Header: header, PathLen: pathLen, Path: append([]byte(nil), r.path...), Payload: payload}
	if r.scope != nil {
		if r.flood {
			p.Header = mesh.MakeHeader(mesh.RouteTransportFlood, payloadType)
		} else {
			p.Header = mesh.MakeHeader(mesh.RouteTransportDirect, payloadType)
		}
		p.TransportCodes[0] = r.scope.Code(payloadType, payload)
	}
	return p
}

// peer plays a MeshCore client.
type peer struct {
	h      *harness
	id     *mesh.Identity
	secret [32]byte
	clock  uint32 // the client's own clock, always increasing
}

func (h *harness) newPeer(seed byte) *peer {
	h.t.Helper()
	return h.newPeerWith(seededIdentity(h.t, seed))
}

func (h *harness) newPeerWith(id *mesh.Identity) *peer {
	h.t.Helper()
	pub := h.id.PublicKey()
	secret, err := id.SharedSecret(pub[:])
	if err != nil {
		h.t.Fatal(err)
	}
	return &peer{h: h, id: id, secret: secret, clock: 1000}
}

func (p *peer) pub() [32]byte { return p.id.PublicKey() }

func (p *peer) tick() uint32 { p.clock++; return p.clock }

// loginPlain builds the login plaintext: timestamp | sync_since | password.
func loginPlain(ts, since uint32, password string) []byte {
	plain := make([]byte, 0, 9+len(password))
	plain = binary.LittleEndian.AppendUint32(plain, ts)
	plain = binary.LittleEndian.AppendUint32(plain, since)
	plain = append(plain, password...)
	return append(plain, 0)
}

func (p *peer) loginPacket(password string, since uint32, r route) mesh.Packet {
	return p.loginPacketAt(p.tick(), password, since, r)
}

func (p *peer) loginPacketAt(ts uint32, password string, since uint32, r route) mesh.Packet {
	pub := p.pub()
	payload, err := mesh.AnonPayload(p.h.id.Hash(), pub[:], p.secret[:], loginPlain(ts, since, password))
	if err != nil {
		p.h.t.Fatal(err)
	}
	return r.packet(mesh.PayloadAnonReq, payload)
}

// textPlain builds TXT_MSG plaintext: timestamp | (type<<2 | attempt) | text.
func textPlain(ts uint32, kind, attempt byte, text string) []byte {
	plain := make([]byte, 0, 5+len(text))
	plain = binary.LittleEndian.AppendUint32(plain, ts)
	plain = append(plain, kind<<2|attempt&3)
	return append(plain, text...)
}

func (p *peer) peerPacket(payloadType byte, plain []byte, r route) mesh.Packet {
	payload, err := mesh.PeerPayload(p.h.id.Hash(), p.id.Hash(), p.secret[:], plain)
	if err != nil {
		p.h.t.Fatal(err)
	}
	return r.packet(payloadType, payload)
}

// text returns a post packet and its plaintext (for ACK computation).
func (p *peer) text(text string, r route) (mesh.Packet, []byte) {
	return p.textAt(p.tick(), mesh.TxtTypePlain, 0, text, r)
}

func (p *peer) textAt(ts uint32, kind, attempt byte, text string, r route) (mesh.Packet, []byte) {
	plain := textPlain(ts, kind, attempt, text)
	return p.peerPacket(mesh.PayloadTxtMsg, plain, r), plain
}

func (p *peer) request(reqType byte, forceSince uint32, r route) (mesh.Packet, []byte) {
	plain := make([]byte, 0, 9)
	plain = binary.LittleEndian.AppendUint32(plain, p.tick())
	plain = append(plain, reqType)
	plain = binary.LittleEndian.AppendUint32(plain, forceSince)
	return p.peerPacket(mesh.PayloadReq, plain, r), plain
}

func (p *peer) keepAlive(forceSince uint32, r route) (mesh.Packet, []byte) {
	return p.request(0x02, forceSince, r)
}

// pathPacket is the PATH a client returns: the route as it received the room's
// flooded packet.
func (p *peer) pathPacket(path []byte, hashSize int, extraType byte, extra []byte, r route) mesh.Packet {
	pathLen, err := mesh.EncodePathLen(hashSize, len(path)/hashSize)
	if err != nil {
		p.h.t.Fatal(err)
	}
	payload, err := mesh.PathReturnPayload(p.h.id.Hash(), p.id.Hash(), p.secret[:], pathLen, path, extraType, extra, rand.New(rand.NewSource(2)))
	if err != nil {
		p.h.t.Fatal(err)
	}
	return r.packet(mesh.PayloadPath, payload)
}

func (p *peer) ackPacket(plain []byte, r route) mesh.Packet {
	ack := mesh.AckHash(plain, p.pubSlice())
	return r.packet(mesh.PayloadAck, ack[:])
}

func (p *peer) pubSlice() []byte { pub := p.pub(); return pub[:] }

// open decrypts a packet the room sent to this peer.
func (p *peer) open(sp sentPacket) []byte {
	p.h.t.Helper()
	payload := sp.Pkt.Payload
	if len(payload) < 5 || payload[0] != p.id.Hash() || payload[1] != p.h.id.Hash() {
		p.h.t.Fatalf("packet not addressed to this peer: %x", payload)
	}
	plain, ok := mesh.MACThenDecrypt(p.secret[:], payload[2:])
	if !ok {
		p.h.t.Fatalf("MAC did not verify for payload %x", payload)
	}
	return plain
}

// login logs the peer in and returns the room's decrypted login response.
func (h *harness) login(p *peer, password string, since uint32, r route) []byte {
	h.t.Helper()
	h.svc.process(p.loginPacket(password, since, r), h.now)
	sp := h.tx.only(h.t)
	switch sp.Pkt.PayloadType() {
	case mesh.PayloadPath:
		plain := p.open(sp)
		_, _, extraType, extra, err := mesh.ParsePathReturn(plain)
		if err != nil || extraType != mesh.PayloadResponse {
			h.t.Fatalf("login PATH return: type=%d err=%v", extraType, err)
		}
		return extra[:13]
	case mesh.PayloadResponse:
		return p.open(sp)[:13]
	}
	h.t.Fatalf("unexpected login reply type %d", sp.Pkt.PayloadType())
	return nil
}

func (h *harness) clientFor(p *peer) *client {
	h.t.Helper()
	c := h.room.byKey[p.pub()]
	if c == nil {
		h.t.Fatal("client not in the room's table")
	}
	return c
}

func (h *harness) postCount() int64 {
	h.t.Helper()
	n, err := h.repo.CountPosts(testContext(), h.cfg.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return n
}
