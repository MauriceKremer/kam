package kiss

import (
	"reflect"
	"testing"
	"time"
)

func TestWriterTxGateWaitsForTxDoneAndPrioritizesControl(t *testing.T) {
	m := &Manager{}
	m.TestInit()
	written := make(chan []byte, 8)
	m.writeHook = func(frame []byte) error {
		written <- append([]byte(nil), frame...)
		return nil
	}
	go m.writeLoop()
	defer close(m.stopChan)

	first := []byte{CMD_DATA, 0xA1}
	second := []byte{CMD_DATA, 0xB2}
	m.TxChan() <- first
	if got := awaitWritten(t, written); !reflect.DeepEqual(got, first) {
		t.Fatalf("first write = %x, want %x", got, first)
	}

	controlWritten := make(chan error, 1)
	m.controlChan <- controlFrame{frame: []byte{CMD_SETHARDWARE, SUB_PING}, done: controlWritten}
	if got := awaitWritten(t, written); !reflect.DeepEqual(got, []byte{CMD_SETHARDWARE, SUB_PING}) {
		t.Fatalf("control write = %x", got)
	}
	if err := <-controlWritten; err != nil {
		t.Fatalf("control write error: %v", err)
	}

	m.TxChan() <- second
	select {
	case got := <-written:
		t.Fatalf("second data frame wrote before TxDone: %x", got)
	case <-time.After(30 * time.Millisecond):
	}
	m.txGateEvents <- txGateEvent{done: true}
	if got := awaitWritten(t, written); !reflect.DeepEqual(got, second) {
		t.Fatalf("second write = %x, want %x", got, second)
	}
}

func TestWriterRetriesTxBusyOnce(t *testing.T) {
	m := &Manager{}
	m.TestInit()
	m.csma = CSMAConfig{}
	m.setAirtime(1000)
	written := make(chan []byte, 4)
	m.writeHook = func(frame []byte) error {
		written <- append([]byte(nil), frame...)
		return nil
	}
	go m.writeLoop()
	defer close(m.stopChan)

	frame := []byte{CMD_DATA, 0xAA}
	m.TxChan() <- frame
	if got := awaitWritten(t, written); !reflect.DeepEqual(got, frame) {
		t.Fatalf("first write = %x, want %x", got, frame)
	}
	m.txGateEvents <- txGateEvent{busy: true}
	if got := awaitWritten(t, written); !reflect.DeepEqual(got, frame) {
		t.Fatalf("retry write = %x, want %x", got, frame)
	}
	if got := m.Counters().TxBusyRetry; got != 1 {
		t.Fatalf("TxBusyRetry = %d, want 1", got)
	}
	m.txGateEvents <- txGateEvent{busy: true}
	m.TxChan() <- []byte{CMD_DATA, 0xBB}
	if got := awaitWritten(t, written); !reflect.DeepEqual(got, []byte{CMD_DATA, 0xBB}) {
		t.Fatalf("post-busy write = %x", got)
	}
}

func TestWriterRejectsInvalidDataLengths(t *testing.T) {
	m := &Manager{}
	m.TestInit()
	written := make(chan []byte, 2)
	m.writeHook = func(frame []byte) error {
		written <- append([]byte(nil), frame...)
		return nil
	}
	go m.writeLoop()
	defer close(m.stopChan)

	m.TxChan() <- []byte{CMD_DATA}
	tooLong := make([]byte, KISSMaxDataSize+2)
	tooLong[0] = CMD_DATA
	m.TxChan() <- tooLong
	m.TxChan() <- []byte{CMD_DATA, 0x01}
	if got := awaitWritten(t, written); !reflect.DeepEqual(got, []byte{CMD_DATA, 0x01}) {
		t.Fatalf("valid frame write = %x", got)
	}
	if got := m.Counters().TxRejectedLength; got != 2 {
		t.Fatalf("TxRejectedLength = %d, want 2", got)
	}
}

func awaitWritten(t *testing.T, written <-chan []byte) []byte {
	t.Helper()
	select {
	case frame := <-written:
		return frame
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for write")
		return nil
	}
}
