// Package room implements a MeshCore room server on top of the KAM packet tap
// and transmit scheduler: logins, roles, posts stored durably before they are
// acknowledged, and push synchronisation to logged-in clients.
//
// One worker goroutine owns every room's state. Packets arrive through
// HandlePacket (a bounded queue), time-driven work runs on a tick, and the
// synchronous process/tick methods let tests drive the service with an
// injected clock. Reference behaviour is MeshCore
// examples/simple_room_server/MyMesh.cpp at the commit pinned in
// docs/FeatureRoomServer.md; deviations are listed there.
package room

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"kam.bytecoder.nl/internal/db"
	"kam.bytecoder.nl/internal/kiss"
	"kam.bytecoder.nl/internal/mesh"
	"kam.bytecoder.nl/internal/models"
)

const (
	// tickInterval is how often time-driven work (pushes, adverts, flushes) runs.
	tickInterval = 50 * time.Millisecond
	// inboundQueue bounds packets waiting for the worker.
	inboundQueue = 64
	// systemPostQueue bounds script posts waiting for the worker.
	systemPostQueue = 16
	// storeTimeout bounds one database call made by the worker.
	storeTimeout = 10 * time.Second
)

// The not-found results of RemoveClient; a system post to an unknown room
// also reports ErrUnknownRoom.
var (
	ErrUnknownRoom   = errors.New("unknown room")
	ErrUnknownClient = errors.New("unknown room client")
)

// Store is the persistence the service needs; *db.RoomRepository implements it.
type Store interface {
	RegisterRoom(ctx context.Context, roomID string, publicKey []byte) (changedFrom []byte, err error)
	AppendPost(ctx context.Context, roomID string, post db.RoomPost, clientKey []byte, clientLastTimestamp uint32) error
	LatestPosts(ctx context.Context, roomID string, limit int) ([]db.RoomPost, error)
	PrunePosts(ctx context.Context, roomID string, keepNewest int, olderThan uint32) (int64, error)
	SaveClients(ctx context.Context, roomID string, clients []db.RoomClient) error
	DeleteClient(ctx context.Context, roomID string, publicKey []byte) error
	LoadClients(ctx context.Context, roomID string) ([]db.RoomClient, error)
}

// Transmitter queues packets for the radio; *kiss.TxScheduler implements it.
type Transmitter interface {
	Submit(kiss.TxRequest) error
	Cancel(tag string) int
}

// Spec is one room to run: its validated configuration and identity.
type Spec struct {
	Config   models.RoomConfig
	Identity *mesh.Identity
}

// Options configures a Service.
type Options struct {
	Rooms       []Spec
	Mesh        models.MeshConfig
	Store       Store
	Transmitter Transmitter
	// Now and Random are injectable for tests; they default to time.Now and
	// crypto/rand.
	Now    func() time.Time
	Random io.Reader
}

type MonitorCounters struct {
	LoginsOK          uint64
	LoginsFailed      uint64
	LoginsRateLimited uint64
	PostsAccepted     uint64
	PostsRejected     uint64
	PushesSent        uint64
	PushesAcked       uint64
	PushTimeouts      uint64
}

type MonitorClient struct {
	PublicKey      string
	Role           uint8
	LastActivityAt *time.Time
	PushState      string
}

type MonitorRoom struct {
	ID             string
	Name           string
	Enabled        bool
	PublicKey      string
	MaxClients     int
	Clients        []MonitorClient
	LastActivityAt *time.Time
	LastPostAt     *time.Time
	Counters       MonitorCounters
}

type monitorSnapshotRequest struct {
	reply chan []MonitorRoom
}

// Stats is a snapshot of service counters.
type Stats struct {
	LoginsOK            uint64
	LoginsFailed        uint64
	LoginsRateLimited   uint64
	LoginsReplayed      uint64
	PostsAccepted       uint64
	PostsRejected       uint64
	PushesSent          uint64
	PushesAcked         uint64
	PushTimeouts        uint64
	UnsupportedRequests uint64
	PacketsDropped      uint64
	TxRejected          uint64
	Clients             int64
}

type counters struct {
	loginsOK, loginsFailed, loginsRateLimited, loginsReplayed atomic.Uint64
	postsAccepted, postsRejected                              atomic.Uint64
	pushesSent, pushesAcked, pushTimeouts                     atomic.Uint64
	unsupportedRequests, packetsDropped, txRejected           atomic.Uint64
}

type region struct {
	name string
	key  mesh.TransportKey
}

// Service runs a set of rooms.
type Service struct {
	store  Store
	tx     Transmitter
	now    func() time.Time
	random io.Reader
	mesh   models.MeshConfig

	regions       []region
	defaultScope  *mesh.TransportKey
	rooms         []*roomState
	limiter       loginLimiter
	lastUnique    uint32
	cnt           counters
	clients       atomic.Int64
	monitorMu     sync.RWMutex
	monitorRecord func(models.MonitorEvent)

	in        chan kiss.RxPacket
	monitor   chan monitorSnapshotRequest
	systemIn  chan systemPostRequest
	removeIn  chan removeClientRequest
	stop      chan struct{}
	done      chan struct{}
	started   atomic.Bool
	startOnce sync.Once
	stopOnce  sync.Once
}

// systemPostRequest is a post a script asked for; reply has capacity one so the
// worker never blocks delivering its verdict.
type systemPostRequest struct {
	roomID string
	text   string
	reply  chan error
}

// removeClientRequest asks the worker to forget one client; reply has capacity
// one so the worker never blocks delivering its verdict.
type removeClientRequest struct {
	roomID string
	key    [32]byte
	reply  chan error
}

// NewService loads each room's stored state and prepares the service. It does
// not transmit or start any goroutine; call Start for that.
func NewService(opts Options) (*Service, error) {
	if len(opts.Rooms) == 0 {
		return nil, errors.New("room: no rooms to run")
	}
	if opts.Store == nil || opts.Transmitter == nil {
		return nil, errors.New("room: store and transmitter are required")
	}
	if err := opts.Mesh.Validate(); err != nil {
		return nil, err
	}
	s := &Service{
		store:    opts.Store,
		tx:       opts.Transmitter,
		now:      opts.Now,
		random:   opts.Random,
		mesh:     opts.Mesh,
		limiter:  newLoginLimiter(),
		in:       make(chan kiss.RxPacket, inboundQueue),
		monitor:  make(chan monitorSnapshotRequest),
		systemIn: make(chan systemPostRequest, systemPostQueue),
		removeIn: make(chan removeClientRequest),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.random == nil {
		s.random = rand.Reader
	}
	for _, name := range opts.Mesh.KnownRegions() {
		s.regions = append(s.regions, region{name: name, key: mesh.RegionKey(name)})
	}
	if opts.Mesh.DefaultScope != "" {
		key := mesh.RegionKey(opts.Mesh.DefaultScope)
		s.defaultScope = &key
	} else {
		slog.Warn("[Room] no default scope configured; adverts and unscoped replies are sent as unscoped floods, which scoped meshes may not carry")
	}

	now := s.now()
	seenHash := map[byte]string{}
	for index, spec := range opts.Rooms {
		if spec.Identity == nil {
			return nil, fmt.Errorf("room %q has no identity", spec.Config.ID)
		}
		if other, dup := seenHash[spec.Identity.Hash()]; dup {
			slog.Warn("[Room] two rooms share the first public-key byte; the first login whose MAC verifies wins", "room", spec.Config.ID, "other", other)
		}
		seenHash[spec.Identity.Hash()] = spec.Config.ID
		r, err := s.loadRoom(spec, index, now)
		if err != nil {
			return nil, fmt.Errorf("load room %q: %w", spec.Config.ID, err)
		}
		s.rooms = append(s.rooms, r)
	}
	return s, nil
}

// Start launches the worker.
func (s *Service) Start() {
	s.startOnce.Do(func() {
		s.started.Store(true)
		go s.run()
	})
}

// Stop stops the worker after it flushes pending client state. Packets still
// queued are discarded. It is safe to call more than once.
func (s *Service) Stop() {
	s.stopOnce.Do(func() { close(s.stop) })
	if s.started.Load() {
		<-s.done
		return
	}
	s.flushAll(s.now())
}

// HandlePacket queues an inbound packet for the worker without blocking; it is
// the kiss.PacketHandler to subscribe. A full queue drops the packet.
func (s *Service) HandlePacket(pkt kiss.RxPacket) {
	select {
	case s.in <- pkt:
	default:
		s.cnt.packetsDropped.Add(1)
	}
}

// Stats returns a snapshot of the counters.
func (s *Service) Stats() Stats {
	return Stats{
		LoginsOK: s.cnt.loginsOK.Load(), LoginsFailed: s.cnt.loginsFailed.Load(),
		LoginsRateLimited: s.cnt.loginsRateLimited.Load(), LoginsReplayed: s.cnt.loginsReplayed.Load(),
		PostsAccepted: s.cnt.postsAccepted.Load(), PostsRejected: s.cnt.postsRejected.Load(),
		PushesSent: s.cnt.pushesSent.Load(), PushesAcked: s.cnt.pushesAcked.Load(), PushTimeouts: s.cnt.pushTimeouts.Load(),
		UnsupportedRequests: s.cnt.unsupportedRequests.Load(), PacketsDropped: s.cnt.packetsDropped.Load(),
		TxRejected: s.cnt.txRejected.Load(), Clients: s.clients.Load(),
	}
}

func (s *Service) MonitorAddresses() map[byte]string {
	addresses := make(map[byte]string, len(s.rooms))
	for _, room := range s.rooms {
		if _, exists := addresses[room.hash]; exists {
			addresses[room.hash] = ""
		} else {
			addresses[room.hash] = room.cfg.ID
		}
	}
	return addresses
}

func (s *Service) SetMonitorRecorder(record func(models.MonitorEvent)) {
	s.monitorMu.Lock()
	s.monitorRecord = record
	s.monitorMu.Unlock()
}

func (s *Service) recordRoomEvent(roomID, event, client, role string, at time.Time) {
	s.monitorMu.RLock()
	record := s.monitorRecord
	s.monitorMu.RUnlock()
	if record == nil {
		return
	}
	roomName, clientName := roomID, client
	value := models.MonitorEvent{Kind: "room", At: at.UTC().Truncate(time.Millisecond), Room: &roomName, Event: event, Client: clientName, Role: role}
	record(value)
}

// MonitorSnapshot copies room state on its owning worker goroutine.
func (s *Service) MonitorSnapshot(ctx context.Context) ([]MonitorRoom, error) {
	reply := make(chan []MonitorRoom, 1)
	select {
	case s.monitor <- monitorSnapshotRequest{reply: reply}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		return nil, errors.New("room service is stopped")
	}
	select {
	case snapshot := <-reply:
		return snapshot, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		return nil, errors.New("room service is stopped")
	}
}

func (s *Service) monitorSnapshot() []MonitorRoom {
	snapshot := make([]MonitorRoom, 0, len(s.rooms))
	for _, r := range s.rooms {
		item := MonitorRoom{ID: r.cfg.ID, Name: r.cfg.Name, Enabled: r.cfg.Enabled, PublicKey: hex.EncodeToString(r.pub[:]), MaxClients: r.cfg.MaxClients, Clients: make([]MonitorClient, 0, len(r.clients)), Counters: MonitorCounters{
			LoginsOK: r.cnt.loginsOK.Load(), LoginsFailed: r.cnt.loginsFailed.Load(), LoginsRateLimited: r.cnt.loginsRateLimited.Load(),
			PostsAccepted: r.cnt.postsAccepted.Load(), PostsRejected: r.cnt.postsRejected.Load(), PushesSent: r.cnt.pushesSent.Load(),
			PushesAcked: r.cnt.pushesAcked.Load(), PushTimeouts: r.cnt.pushTimeouts.Load(),
		}}
		for _, c := range r.clients {
			state := "idle"
			if c.failures >= maxPushFailures {
				state = "paused"
			} else if c.hasPending {
				state = "awaiting_ack"
			}
			lastHeard := optionalTime(c.lastActivity)
			item.Clients = append(item.Clients, MonitorClient{PublicKey: hex.EncodeToString(c.pub[:]), Role: c.role, LastActivityAt: lastHeard, PushState: state})
			if lastHeard != nil && (item.LastActivityAt == nil || lastHeard.After(*item.LastActivityAt)) {
				stamp := *lastHeard
				item.LastActivityAt = &stamp
			}
		}
		if len(r.posts) > 0 {
			lastPost := r.posts[len(r.posts)-1].CreatedAt
			if !lastPost.IsZero() {
				item.LastPostAt = &lastPost
				if item.LastActivityAt == nil || lastPost.After(*item.LastActivityAt) {
					stamp := lastPost
					item.LastActivityAt = &stamp
				}
			}
		}
		snapshot = append(snapshot, item)
	}
	return snapshot
}

func (s *Service) run() {
	defer close(s.done)
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			s.flushAll(s.now())
			return
		case pkt := <-s.in:
			s.process(pkt.Packet, s.now())
		case request := <-s.monitor:
			request.reply <- s.monitorSnapshot()
		case request := <-s.systemIn:
			request.reply <- s.addSystemPost(request.roomID, request.text, s.now())
		case request := <-s.removeIn:
			request.reply <- s.forgetClient(request.roomID, request.key, s.now())
		case <-ticker.C:
			s.tick(s.now())
		}
	}
}

// process handles one inbound packet. Anything malformed or not addressed to a
// room is dropped silently.
func (s *Service) process(pkt mesh.Packet, now time.Time) {
	switch pkt.PayloadType() {
	case mesh.PayloadAck:
		if len(pkt.Payload) >= 4 {
			s.handleAck(pkt.Payload[:4])
		}
	case mesh.PayloadMultipart:
		// remaining<<4 | inner type, then the inner payload; only ACKs are used.
		if len(pkt.Payload) >= 5 && pkt.Payload[0]&0x0F == mesh.PayloadAck {
			s.handleAck(pkt.Payload[1:5])
		}
	case mesh.PayloadReq, mesh.PayloadTxtMsg, mesh.PayloadPath:
		if s.forUs(pkt) {
			s.handlePeer(pkt, now)
		}
	case mesh.PayloadAnonReq:
		if s.forUs(pkt) {
			s.handleAnon(pkt, now)
		}
	}
}

// forUs applies Mesh::onRecvPacket: a direct packet that still has hops left
// belongs to a forwarder, not to its final destination.
func (s *Service) forUs(pkt mesh.Packet) bool {
	return !(pkt.IsDirect() && pkt.HashCount() > 0)
}

// AddSystemPost stores a post authored by the room itself. It is safe from any
// goroutine: the request is handed to the worker, which owns room state. The
// caller's context bounds the wait for the verdict, but the worker may still
// store the post after a context error.
func (s *Service) AddSystemPost(ctx context.Context, roomID, text string) error {
	request := systemPostRequest{roomID: roomID, text: text, reply: make(chan error, 1)}
	select {
	case <-s.stop:
		return errors.New("room service is stopped")
	default:
	}
	select {
	case s.systemIn <- request:
	default:
		return errors.New("system post queue is full")
	}
	select {
	case err := <-request.reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-s.stop:
		return errors.New("room service is stopped")
	}
}

// RemoveClient forgets one remembered client of a room: its stored record, its
// session and any push still queued for it. It is not a ban; the client can log
// in again under the room's password rules. The worker deletes the stored
// record first, so a failed delete keeps the session and is returned. Unknown
// rooms and clients report ErrUnknownRoom and ErrUnknownClient. Once the worker
// has taken the request it finishes it, even if ctx ends first.
func (s *Service) RemoveClient(ctx context.Context, roomID string, publicKey [32]byte) error {
	request := removeClientRequest{roomID: roomID, key: publicKey, reply: make(chan error, 1)}
	select {
	case s.removeIn <- request:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return errors.New("room service is stopped")
	}
	// The worker replies before it can stop, so only ctx can end this wait early.
	select {
	case err := <-request.reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) tick(now time.Time) {
	for _, r := range s.rooms {
		r.tick(now)
	}
}

func (s *Service) flushAll(now time.Time) {
	for _, r := range s.rooms {
		r.flushClients(now, true)
	}
}

func (s *Service) roomByID(id string) *roomState {
	for _, r := range s.rooms {
		if r.cfg.ID == id {
			return r
		}
	}
	return nil
}

// uniqueNow returns the current time in unix seconds, bumped by one when it
// would repeat or go backwards within a run (RTCClock::getCurrentTimeUnique).
func (s *Service) uniqueNow(now time.Time) uint32 {
	t := uint32(now.Unix())
	if t <= s.lastUnique {
		t = s.lastUnique + 1
	}
	s.lastUnique = t
	return t
}

func (s *Service) randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := io.ReadFull(s.random, b); err != nil {
		slog.Error("[Room] random source failed", "error", err)
	}
	return b
}

func storeContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), storeTimeout)
}

// shortKey formats the first bytes of a public key for logs; full keys and
// passwords are never logged.
func shortKey(pub []byte) string {
	if len(pub) < 4 {
		return hex.EncodeToString(pub)
	}
	return hex.EncodeToString(pub[:4])
}
