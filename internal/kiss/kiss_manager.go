package kiss

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"go.bug.st/serial"

	"kam.bytecoder.nl/internal/kiss/parsers"
)

// defaultRxMetaTimeout is how long the router waits for an RxMeta frame after a
// group text before dispatching without radio metadata. The KISS modem sends
// RxMeta immediately after each data frame, so this is a safety bound.
const defaultRxMetaTimeout = 200 * time.Millisecond

// Constants for KISS protocol framing
const (
	FEND  = 0xC0
	FESC  = 0xDB
	TFEND = 0xDC
	TFESC = 0xDD

	CMD_DATA        = 0x00
	CMD_SETHARDWARE = 0x06

	SUB_GET_RADIO  = 0x0B
	SUB_RESP_RADIO = 0x8B
	SUB_SET_RADIO  = 0x09
	SUB_RESP_OK    = 0xF0
	SUB_RX_META    = 0xF9 // unsolicited RxMeta: SNR (1, signed, x4) + RSSI (1, signed dBm)
)

type Manager struct {
	stream serial.Port
	txChan chan []byte
	rxChan chan []byte

	registry *ResponseRegistry

	// hardware Tx Mutex protect against concurrent writes to the serial port
	hwTxMutex sync.Mutex

	// stopChan is used to signal the goroutines to stop
	stopChan chan struct{}

	// NodeName is the local sender name used in outbound group texts.
	NodeName string

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

	// The fixed-size FIFO queue of recently seen message hashes for duplicate suppression
	seenHashes []string

	// Store receives parsed packets for asynchronous persistence. May be nil.
	Store Persistence
}

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
		registry:      NewResponseRegistry(),
		stopChan:      make(chan struct{}),
		rxMetaTimeout: defaultRxMetaTimeout,
	}, nil
}

func (m *Manager) Start() {
	// Start the goroutines
	go m.readLoop()                      // located in kiss_reader.go
	go m.writeLoop()                     // located in kiss_writer.go
	go m.routeLoop(context.Background()) // located in kiss_router.go
}

func (m *Manager) TxChan() chan<- []byte {
	return m.txChan
}

// TestInit initializes minimal internal channels for unit tests.
func (m *Manager) TestInit() {
	m.txChan = make(chan []byte, 1)
	m.rxChan = make(chan []byte, 10)
	m.registry = NewResponseRegistry()
	m.stopChan = make(chan struct{})
	m.rxMetaTimeout = defaultRxMetaTimeout
}

// TestTxChan returns the internal txChan for tests.
func (m *Manager) TestTxChan() <-chan []byte {
	return m.txChan
}

// TestRxChan returns a send-only view of the internal rxChan so tests can feed
// inbound frames into the route loop without a serial port.
func (m *Manager) TestRxChan() chan<- []byte {
	return m.rxChan
}

func (m *Manager) Stop() {
	// Signal the goroutines to stop
	close(m.stopChan)

	// Flush any pending packet writes before closing the database handle
	if m.Store != nil {
		if err := m.Store.Close(); err != nil {
			slog.Error("[Manager] failed to close persistence store", "error", err)
		}
	}

	// Close the serial port
	m.stream.Close()
}

type RadioConfig struct {
	Frequency       uint32 // e.g., 868000000 Hz
	Bandwidth       uint32 // e.g., 125000 Hz
	SpreadingFactor uint8  // e.g., 7 to 12
	CodingRate      uint8  // e.g., 5 to 8
}

func (m *Manager) LockDevice() {
	m.hwTxMutex.Lock()
}

func (m *Manager) UnlockDevice() {
	m.hwTxMutex.Unlock()
}
