package kiss

import (
	"context"
	"log/slog"
	"sync"

	"go.bug.st/serial"
)

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

	// The fixed-size FIFO queue of recently seen message hashes for duplicate suppression
	seenHashes []string

	// AdvertStore receives validated advert packets asynchronously. May be nil.
	AdvertStore AdvertPersistence
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
		stream:   stream,
		txChan:   make(chan []byte, 10), // Buffered to prevent immediate blocking
		rxChan:   make(chan []byte, 10),
		registry: NewResponseRegistry(),
		stopChan: make(chan struct{}),
	}, nil
}

func (m *Manager) Start() {
	// Start the goroutines
	go m.readLoop()                      // located in kiss_reader.go
	go m.writeLoop()                     // located in kiss_writer.go
	go m.routeLoop(context.Background()) // located in kiss_router.go
}

func (m *Manager) Stop() {
	// Signal the goroutines to stop
	close(m.stopChan)

	// Flush any pending advert writes before closing the database handle
	if m.AdvertStore != nil {
		if err := m.AdvertStore.Close(); err != nil {
			slog.Error("[Manager] failed to close advert store", "error", err)
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
