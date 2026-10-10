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
	// storeTimeout bounds one database call made by the worker.
	storeTimeout = 10 * time.Second
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

	regions      []region
	defaultScope *mesh.TransportKey
	rooms        []*roomState
	limiter      loginLimiter
	lastUnique   uint32
	cnt          counters
	clients      atomic.Int64

	in        chan kiss.RxPacket
	stop      chan struct{}
	done      chan struct{}
	started   atomic.Bool
	startOnce sync.Once
	stopOnce  sync.Once
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
		store:   opts.Store,
		tx:      opts.Transmitter,
		now:     opts.Now,
		random:  opts.Random,
		mesh:    opts.Mesh,
		limiter: newLoginLimiter(),
		in:      make(chan kiss.RxPacket, inboundQueue),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
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
