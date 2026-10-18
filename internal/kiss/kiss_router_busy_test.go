package kiss

// Regression test for D9 (docs/FeatureKissModem.md §7), written after the
// 2026-10-18 hardware smoke run: a TxBusy reply that a pending request
// consumes must also reach the writer's TX gate, or the in-flight data frame
// dies at the gate timeout instead of being resent.

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRouterMirrorsTxBusyToGateWhileRequestPending(t *testing.T) {
	m := &Manager{}
	m.TestInit()
	m.csma = CSMAConfig{}
	m.setAirtime(1000) // 3 s gate timeout, so the 500 ms backoff wins
	written := make(chan []byte, 4)
	m.writeHook = func(frame []byte) error {
		written <- append([]byte(nil), frame...)
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.routeLoop(ctx)
	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		m.writeLoop()
	}()
	defer func() {
		close(m.stopChan)
		<-writeDone
	}()

	// A data frame goes in flight through the writer gate.
	dataFrame := []byte{CMD_DATA, 0x25, 0, 1, 2, 3}
	m.TxChan() <- dataFrame
	if got := awaitWritten(t, written); string(got) != string(dataFrame) {
		t.Fatalf("first write = %x, want %x", got, dataFrame)
	}

	// A control request claims the request slot. It is registered before its
	// frame is written, so by the time the frame appears the slot is held.
	result := make(chan error, 1)
	go func() {
		_, err := NewRadioClient(m).GetRadio(context.Background())
		result <- err
	}()
	if got := awaitWritten(t, written); len(got) != 2 || got[1] != SUB_GET_RADIO {
		t.Fatalf("request frame = %x, want GetRadio", got)
	}

	// The modem answers the request with Error/TxBusy. Observed on hardware:
	// the busy reply to a second frame while one is pending, 3/3 attempts.
	select {
	case m.TestRxChan() <- []byte{CMD_SETHARDWARE, SUB_ERROR, MODEM_ERROR_TX_BUSY}:
	case <-time.After(time.Second):
		t.Fatal("busy error was not routed")
	}

	select {
	case err := <-result:
		if !errors.Is(err, ErrTxBusy) {
			t.Fatalf("request error = %v, want ErrTxBusy", err)
		}
	case <-time.After(time.Second):
		t.Fatal("request did not receive the busy error")
	}

	// The writer must see the mirrored busy event and resend the frame after
	// the 500 ms backoff instead of dropping it at the gate timeout.
	if got := awaitWritten(t, written); string(got) != string(dataFrame) {
		t.Fatalf("resent frame = %x, want %x", got, dataFrame)
	}
	select {
	case event := <-m.eventChan:
		if _, ok := event.(TxBusy); !ok {
			t.Fatalf("modem event = %#v, want TxBusy", event)
		}
	case <-time.After(time.Second):
		t.Fatal("no TxBusy published to modem event subscribers")
	}
	time.Sleep(600 * time.Millisecond)
	if got := m.counters.txGateTimeout.Load(); got != 0 {
		t.Fatalf("tx_gate_timeout = %d, want 0", got)
	}
}
