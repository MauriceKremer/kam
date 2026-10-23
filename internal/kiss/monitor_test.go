package kiss

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"kam.bytecoder.nl/internal/events"
	"kam.bytecoder.nl/internal/models"
	"kam.bytecoder.nl/internal/monitor"
)

func monitoredManager(t *testing.T) (*Manager, *events.Ring) {
	t.Helper()
	m := &Manager{}
	m.TestInit()
	ring := events.NewRing(32)
	m.SetMonitor(ring, models.RadioConfig{FrequencyHz: 868000000, BandwidthHz: 125000, SpreadingFactor: 8, CodingRate: 5})
	return m, ring
}

func monitorPage(t *testing.T, ring *events.Ring) events.Page {
	t.Helper()
	response := httptest.NewRecorder()
	monitor.NewHandler(ring, func() monitor.Status { return monitor.Status{} }).ServeHTTP(response, httptest.NewRequest("GET", "http://127.0.0.1/v1/events", nil))
	var page events.Page
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	return page
}

func TestMonitorObservesUnhandledAndDuplicateRX(t *testing.T) {
	m, ring := monitoredManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.routeLoop(ctx) }()
	defer func() { cancel(); <-done; m.Stop() }()
	for range 2 {
		m.TestRxChan() <- []byte{CMD_DATA, 0x25, 0, 1, 2}
		m.TestRxChan() <- []byte{CMD_SETHARDWARE, SUB_RX_META, 34, 159}
	}
	deadline := time.After(time.Second)
	for {
		page := monitorPage(t, ring)
		if len(page.Events) == 2 {
			if page.Events[0].PayloadType != "TRACE" || page.Events[0].Duplicate || !page.Events[1].Duplicate || page.Events[0].SNRDB == nil || *page.Events[0].SNRDB != 8.5 || *page.Events[0].RSSIDBM != -97 || page.Events[0].AirtimeMS == nil {
				t.Fatalf("RX events: %+v", page.Events)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("RX events not published")
		default:
		}
	}
}

func TestMonitorTXCompletionAndUnknownAirtime(t *testing.T) {
	m, ring := monitoredManager(t)
	written := make(chan []byte, 4)
	m.writeHook = func(frame []byte) error { written <- frame; return nil }
	done := make(chan struct{})
	go func() { defer close(done); m.writeLoop() }()
	defer func() { m.Stop(); <-done }()
	for _, success := range []byte{1, 0} {
		m.TxChan() <- []byte{CMD_DATA, 0x0d, 0, 1, 2, 3, 4}
		awaitWritten(t, written)
		m.dispatchHardwareResponse(SUB_TX_DONE, []byte{success})
	}
	deadline := time.After(time.Second)
	for {
		page := monitorPage(t, ring)
		if len(page.Events) == 2 {
			if page.Events[0].Kind != "tx" || page.Events[0].OK == nil || !*page.Events[0].OK || page.Events[0].AirtimeMS == nil || page.Events[1].OK == nil || *page.Events[1].OK || page.Events[1].AirtimeMS != nil {
				t.Fatalf("TX events: %+v", page.Events)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("TX events not published")
		default:
		}
	}
}

func TestMonitorMissingMetadataAndAbandonedTX(t *testing.T) {
	m, ring := monitoredManager(t)
	m.rxMetaTimeout = time.Millisecond
	m.routePacket([]byte{0x25, 0, 1, 2})
	deadline := time.After(time.Second)
	for len(monitorPage(t, ring).Events) == 0 {
		select {
		case <-deadline:
			t.Fatal("metadata timeout did not publish")
		default:
		}
	}
	if event := monitorPage(t, ring).Events[0]; event.SNRDB != nil || event.RSSIDBM != nil {
		t.Fatal("missing signal metadata must be null")
	}
	m.csma = CSMAConfig{}
	m.setAirtime(1)
	written := make(chan []byte, 1)
	m.writeHook = func(frame []byte) error { written <- frame; return nil }
	done := make(chan struct{})
	go func() { defer close(done); m.writeLoop() }()
	defer func() { m.Stop(); <-done }()
	if !m.TryTransmit([]byte{CMD_DATA, 0x0d, 0, 1, 2, 3, 4}, "room", "lobby") {
		t.Fatal("TX rejected")
	}
	awaitWritten(t, written)
	deadline = time.After(time.Second)
	for {
		page := monitorPage(t, ring)
		if len(page.Events) == 2 {
			event := page.Events[1]
			if event.OK != nil || event.AirtimeMS != nil || event.Origin != "room" || event.Room == nil || *event.Room != "lobby" {
				t.Fatalf("abandoned event: %+v", event)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("abandoned TX not published")
		default:
		}
	}
}
