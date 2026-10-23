package kiss

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"go.bug.st/serial"

	"kam.bytecoder.nl/internal/kiss/parsers"
	"kam.bytecoder.nl/internal/mesh"
	"kam.bytecoder.nl/internal/models"
)

// defaultRxMetaTimeout is how long the router waits for an RxMeta frame after a
// group text before dispatching without radio metadata. The KISS modem sends
// RxMeta immediately after each data frame, so this is a safety bound.
const defaultRxMetaTimeout = 200 * time.Millisecond

type requestResult struct {
	command byte
	payload []byte
	err     error
}

type pendingRequest struct {
	expected map[byte]struct{}
	result   chan requestResult
}

type controlFrame struct {
	frame []byte
	done  chan error
}

type ModemEvent interface{ modemEvent() }

type TxDone struct{ OK bool }
type TxBusy struct{}
type ModemFault struct{ Code byte }

// TxAbandoned reports that the writer gave up on a data frame without ever
// seeing TxDone: repeated TxBusy or a gate timeout. Like TxDone it means the
// modem is free for the next frame.
type TxAbandoned struct{}

func (TxDone) modemEvent()      {}
func (TxBusy) modemEvent()      {}
func (ModemFault) modemEvent()  {}
func (TxAbandoned) modemEvent() {}

type ModemEventHandler func(ModemEvent)

type ModemCounters struct {
	TxDoneOK          uint64
	TxDoneFailed      uint64
	TxBusy            uint64
	TxBusyRetry       uint64
	TxGateTimeout     uint64
	TxRejectedLength  uint64
	RxUnmatchedFrames uint64
}

type modemCounters struct {
	txDoneOK          atomic.Uint64
	txDoneFailed      atomic.Uint64
	txBusy            atomic.Uint64
	txBusyRetry       atomic.Uint64
	txGateTimeout     atomic.Uint64
	txRejectedLength  atomic.Uint64
	rxUnmatchedFrames atomic.Uint64
}

type txGateEvent struct {
	busy bool
	done bool
	ok   bool
}

type Manager struct {
	observer    atomic.Pointer[monitorObserver]
	radioModel  atomic.Pointer[models.RadioConfig]
	stream      serial.Port
	writeHook   func([]byte) error
	txChan      chan []byte
	rxChan      chan []byte
	controlChan chan controlFrame

	requestGate chan struct{}
	requestMu   sync.Mutex
	pending     *pendingRequest
	unsynced    bool
	barrier     bool

	// stopChan is used to signal the goroutines to stop
	stopChan  chan struct{}
	stopOnce  sync.Once
	doneChan  chan struct{}
	doneOnce  sync.Once
	linkErrMu sync.RWMutex
	linkErr   error

	// NodeName is the local sender name used in outbound group texts.
	NodeName string

	csmaMu       sync.RWMutex
	csma         CSMAConfig
	airtimeMu    sync.RWMutex
	airtimeMS    uint32
	counters     modemCounters
	txGateEvents chan txGateEvent

	eventMu       sync.RWMutex
	eventHandlers []ModemEventHandler
	eventChan     chan ModemEvent

	// onGroupTextMu guards onGroupText. The handler slice is read on the
	// routeLoop goroutine and mutated by subscribers, so it needs its own lock
	// independent of hwTxMutex.
	onGroupTextMu sync.RWMutex
	onGroupText   []GroupTextHandler

	// rxMetaTimeout bounds how long the router waits for an RxMeta (0xF9)
	// frame after a group text before dispatching without radio metadata.
	rxMetaTimeout time.Duration

	// pendingMu guards the pending group text awaiting its RxMeta frame.
	// RxMeta follows each data frame per the KISS protocol; we hold the parsed
	// message briefly to attach SNR/RSSI before dispatching to Store + tap.
	pendingMu        sync.Mutex
	pendingGroupText *parsers.GroupTextMessage
	pendingTimer     *time.Timer

	// dedup suppresses duplicate packets over a bounded, time-limited window.
	dedup dedupCache

	// onPacketMu guards onPacket, the room-server packet tap subscribers.
	onPacketMu sync.RWMutex
	onPacket   []PacketHandler

	// Store receives parsed packets for asynchronous persistence. May be nil.
	Store Persistence
}

// RxPacket is a validated inbound packet delivered to packet-tap subscribers:
// route type, transport codes, path and payload are all preserved.
type RxPacket struct {
	mesh.Packet
	ReceivedAt time.Time
}

// PacketHandler observes an inbound peer packet (REQ, TXT_MSG, ACK, ANON_REQ,
// PATH, MULTIPART). Handlers run on the route loop after duplicate suppression
// and must not block.
type PacketHandler func(RxPacket)

// SubscribePackets registers a packet-tap observer. Safe to call before Start.
// The returned cancel function removes the handler.
func (m *Manager) SubscribePackets(h PacketHandler) (cancel func()) {
	m.onPacketMu.Lock()
	defer m.onPacketMu.Unlock()
	m.onPacket = append(m.onPacket, h)
	idx := len(m.onPacket) - 1
	return func() {
		m.onPacketMu.Lock()
		defer m.onPacketMu.Unlock()
		if idx < len(m.onPacket) {
			m.onPacket[idx] = nil
		}
	}
}

func (m *Manager) dispatchPacket(pkt RxPacket) {
	m.onPacketMu.RLock()
	handlers := append([]PacketHandler(nil), m.onPacket...)
	m.onPacketMu.RUnlock()
	for _, h := range handlers {
		if h != nil {
			h(pkt)
		}
	}
}

// MarkSeen records a raw packet this node is about to transmit so an echo of it
// from a repeater is dropped as a duplicate instead of being processed.
func (m *Manager) MarkSeen(raw []byte) {
	if pkt, err := mesh.Parse(raw); err == nil {
		m.dedup.Mark(pkt.PayloadType(), pkt.Payload)
	}
}

// TxGateTimeout returns how long a data frame may stay in flight before the
// writer gives up waiting for TxDone.
func (m *Manager) TxGateTimeout() time.Duration { return m.txGateTimeout() }

// GroupTextHandler observes a decrypted inbound group text. Handlers are
// invoked from the route loop after persistence enqueue; they must not block.
type GroupTextHandler func(msg parsers.GroupTextMessage)

// SubscribeGroupText registers an inbound group text observer. Safe to call
// before Start. The returned cancel function removes the handler.
func (m *Manager) SubscribeGroupText(h GroupTextHandler) (cancel func()) {
	m.onGroupTextMu.Lock()
	defer m.onGroupTextMu.Unlock()
	m.onGroupText = append(m.onGroupText, h)
	idx := len(m.onGroupText) - 1
	return func() {
		m.onGroupTextMu.Lock()
		defer m.onGroupTextMu.Unlock()
		if idx < len(m.onGroupText) && m.onGroupText[idx] != nil {
			m.onGroupText[idx] = nil
		}
	}
}

// dispatchGroupText persists (if a Store is configured) and then invokes all
// subscribed group text handlers. Called once per inbound group text, from the
// route loop's hold/attach/flush paths. nil entries from unsubscribed handlers
// are skipped.
func (m *Manager) dispatchGroupText(msg parsers.GroupTextMessage) {
	if m.Store != nil {
		m.Store.EnqueueGroupText(msg)
	}
	m.onGroupTextMu.RLock()
	handlers := append([]GroupTextHandler(nil), m.onGroupText...)
	m.onGroupTextMu.RUnlock()
	for _, h := range handlers {
		if h != nil {
			h(msg)
		}
	}
}

// holdGroupText stages a parsed group text for dispatch, waiting briefly for
// the modem's RxMeta (0xF9) frame so SNR/RSSI can be attached. If a previous
// message is still pending (its RxMeta was missed), it is flushed first so no
// message is lost.
func (m *Manager) holdGroupText(msg parsers.GroupTextMessage) {
	m.pendingMu.Lock()
	var prev *parsers.GroupTextMessage
	if m.pendingGroupText != nil {
		prev = m.pendingGroupText
		if m.pendingTimer != nil {
			m.pendingTimer.Stop()
		}
	}
	m.pendingGroupText = &msg
	to := m.rxMetaTimeout
	if to <= 0 {
		to = defaultRxMetaTimeout
	}
	m.pendingTimer = time.AfterFunc(to, m.flushPendingGroupText)
	m.pendingMu.Unlock()

	if prev != nil {
		m.dispatchGroupText(*prev)
	}
}

// attachRxMeta attaches modem RX quality to the pending group text and
// dispatches it immediately. No-op when no message is pending (e.g. orphan
// RxMeta for a non-group-text data frame).
func (m *Manager) attachRxMeta(snr, rssi int) {
	m.attachObservation(snr, rssi)
	m.pendingMu.Lock()
	p := m.pendingGroupText
	if p != nil {
		if m.pendingTimer != nil {
			m.pendingTimer.Stop()
		}
		m.pendingGroupText = nil
	}
	m.pendingMu.Unlock()

	if p == nil {
		return
	}
	p.HasRxMeta = true
	p.SNR = snr
	p.RSSI = rssi
	m.dispatchGroupText(*p)
}

// flushPendingGroupText dispatches the pending group text without RxMeta when
// the modem did not emit (or we did not receive) an RxMeta frame in time.
func (m *Manager) flushPendingGroupText() {
	m.pendingMu.Lock()
	p := m.pendingGroupText
	m.pendingGroupText = nil
	m.pendingMu.Unlock()
	if p != nil {
		m.dispatchGroupText(*p)
	}
}

func NewManager(portName string, baud int) (*Manager, error) {
	config := &serial.Mode{
		BaudRate: baud,
	}
	stream, err := serial.Open(portName, config)
	if err != nil {
		return nil, err
	}

	return &Manager{
		stream:        stream,
		txChan:        make(chan []byte, 10), // Buffered to prevent immediate blocking
		rxChan:        make(chan []byte, 10),
		controlChan:   make(chan controlFrame, 10),
		requestGate:   make(chan struct{}, 1),
		txGateEvents:  make(chan txGateEvent, 4),
		eventChan:     make(chan ModemEvent, 32),
		stopChan:      make(chan struct{}),
		doneChan:      make(chan struct{}),
		rxMetaTimeout: defaultRxMetaTimeout,
		csma:          CSMAConfig{TxDelayMS: 500, Persistence: 63, SlotTimeMS: 100},
	}, nil
}

func (m *Manager) Start() {
	// Start the goroutines
	go m.readLoop()                      // located in kiss_reader.go
	go m.writeLoop()                     // located in kiss_writer.go
	go m.routeLoop(context.Background()) // located in kiss_router.go
	go m.modemEventLoop()
}

func (m *Manager) TxChan() chan<- []byte {
	return m.txChan
}

func (m *Manager) Done() <-chan struct{} { return m.doneChan }

func (m *Manager) Err() error {
	m.linkErrMu.RLock()
	defer m.linkErrMu.RUnlock()
	return m.linkErr
}

func (m *Manager) Fail(err error) {
	if err == nil {
		return
	}
	m.doneOnce.Do(func() {
		m.linkErrMu.Lock()
		m.linkErr = err
		m.linkErrMu.Unlock()
		close(m.doneChan)
	})
}

func (m *Manager) SubscribeModemEvents(handler ModemEventHandler) (cancel func()) {
	m.eventMu.Lock()
	m.eventHandlers = append(m.eventHandlers, handler)
	index := len(m.eventHandlers) - 1
	m.eventMu.Unlock()
	return func() {
		m.eventMu.Lock()
		defer m.eventMu.Unlock()
		if index < len(m.eventHandlers) && m.eventHandlers[index] != nil {
			m.eventHandlers[index] = nil
		}
	}
}

func (m *Manager) modemEventLoop() {
	for {
		select {
		case <-m.stopChan:
			return
		case event := <-m.eventChan:
			m.eventMu.RLock()
			handlers := append([]ModemEventHandler(nil), m.eventHandlers...)
			m.eventMu.RUnlock()
			for _, handler := range handlers {
				if handler != nil {
					handler(event)
				}
			}
		}
	}
}

func (m *Manager) publishModemEvent(event ModemEvent) {
	select {
	case m.eventChan <- event:
	default:
		slog.Warn("modem event queue full; event dropped", "event", event)
	}
}

func (m *Manager) Counters() ModemCounters {
	return ModemCounters{
		TxDoneOK: m.counters.txDoneOK.Load(), TxDoneFailed: m.counters.txDoneFailed.Load(),
		TxBusy: m.counters.txBusy.Load(), TxBusyRetry: m.counters.txBusyRetry.Load(),
		TxGateTimeout: m.counters.txGateTimeout.Load(), TxRejectedLength: m.counters.txRejectedLength.Load(),
		RxUnmatchedFrames: m.counters.rxUnmatchedFrames.Load(),
	}
}

func (m *Manager) markUnsynced() {
	m.requestMu.Lock()
	m.unsynced = true
	m.requestMu.Unlock()
}

func (m *Manager) setCSMA(config CSMAConfig) {
	m.csmaMu.Lock()
	m.csma = config
	m.csmaMu.Unlock()
}

func (m *Manager) csmaConfig() CSMAConfig {
	m.csmaMu.RLock()
	defer m.csmaMu.RUnlock()
	return m.csma
}

// CurrentCSMA returns the host's view of the modem's KISS CSMA parameters: the
// values last set through this connection, never a hardware readback.
func (m *Manager) CurrentCSMA() CSMAConfig { return m.csmaConfig() }

func (m *Manager) setAirtime(milliseconds uint32) {
	m.airtimeMu.Lock()
	m.airtimeMS = milliseconds
	m.airtimeMu.Unlock()
}

func (m *Manager) txGateTimeout() time.Duration {
	cfg := m.csmaConfig()
	m.airtimeMu.RLock()
	airtime := m.airtimeMS
	m.airtimeMu.RUnlock()
	if airtime == 0 {
		airtime = 1000
	}
	totalMS := cfg.TxDelayMS + 10*cfg.SlotTimeMS + 3*int(airtime)
	return time.Duration(totalMS) * time.Millisecond
}

// TestInit initializes minimal internal channels for unit tests.
func (m *Manager) TestInit() {
	m.txChan = make(chan []byte, 1)
	m.rxChan = make(chan []byte, 10)
	m.controlChan = make(chan controlFrame, 10)
	m.requestGate = make(chan struct{}, 1)
	m.txGateEvents = make(chan txGateEvent, 4)
	m.eventChan = make(chan ModemEvent, 32)
	m.stopChan = make(chan struct{})
	m.doneChan = make(chan struct{})
	m.rxMetaTimeout = defaultRxMetaTimeout
	m.csma = CSMAConfig{TxDelayMS: 500, Persistence: 63, SlotTimeMS: 100}
}

// TestTxChan returns the internal txChan for tests.
func (m *Manager) TestTxChan() <-chan []byte {
	return m.txChan
}

func (m *Manager) TestControlChan() <-chan controlFrame { return m.controlChan }

// TestRxChan returns a send-only view of the internal rxChan so tests can feed
// inbound frames into the route loop without a serial port.
func (m *Manager) TestRxChan() chan<- []byte {
	return m.rxChan
}

// TestRouteLoop runs the route loop until ctx is cancelled or the manager stops,
// so tests in other packages can drive frames through TestRxChan.
func (m *Manager) TestRouteLoop(ctx context.Context) {
	m.routeLoop(ctx)
}

func (m *Manager) Stop() {
	m.stopOnce.Do(func() {
		m.flushObservation()
		close(m.stopChan)
		if m.Store != nil {
			if err := m.Store.Close(); err != nil {
				slog.Error("[Manager] failed to close persistence store", "error", err)
			}
		}
		if m.stream != nil {
			_ = m.stream.Close()
		}
	})
}

type RadioConfig struct {
	Frequency       uint32 // e.g., 868000000 Hz
	Bandwidth       uint32 // e.g., 125000 Hz
	SpreadingFactor uint8  // e.g., 5 to 12
	CodingRate      uint8  // e.g., 5 to 8
}
