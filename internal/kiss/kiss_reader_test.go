package kiss

import (
	"io"
	"sync"
	"testing"
	"time"

	"go.bug.st/serial"
)

// fakeStream feeds scripted chunks to the reader loop: each Read takes up to
// len(p) bytes from the front of the script, like a real serial port. When the
// script runs out, Read returns EOF (readLoop then stops itself via Fail).
type fakeStream struct {
	mu   sync.Mutex
	pend [][]byte
}

func newFakeStream(chunks ...[]byte) *fakeStream {
	return &fakeStream{pend: chunks}
}

func (f *fakeStream) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pend) == 0 {
		return 0, io.EOF
	}
	c := f.pend[0]
	n := copy(p, c)
	if n == len(c) {
		f.pend = f.pend[1:]
	} else {
		f.pend[0] = c[n:]
	}
	return n, nil
}

// Unused by readLoop; serial.Port requires them.
func (f *fakeStream) SetMode(*serial.Mode) error                           { return nil }
func (f *fakeStream) Write(p []byte) (int, error)                          { return len(p), nil }
func (f *fakeStream) Drain() error                                         { return nil }
func (f *fakeStream) SetDTR(bool) error                                    { return nil }
func (f *fakeStream) Close() error                                         { return nil }
func (f *fakeStream) ResetInputBuffer() error                              { return nil }
func (f *fakeStream) ResetOutputBuffer() error                             { return nil }
func (f *fakeStream) SetRTS(bool) error                                    { return nil }
func (f *fakeStream) GetModemStatusBits() (*serial.ModemStatusBits, error) { return &serial.ModemStatusBits{}, nil }
func (f *fakeStream) SetReadTimeout(time.Duration) error                   { return nil }
func (f *fakeStream) Break(time.Duration) error                            { return nil }

// runReader starts the read loop on a scripted stream and stops it on cleanup.
func runReader(t *testing.T, stream *fakeStream) *Manager {
	t.Helper()
	m := &Manager{}
	m.TestInit()
	m.stream = stream
	go m.readLoop()
	t.Cleanup(m.Stop)
	return m
}

// expectFrame waits for one frame produced by the reader. TestRxChan is
// send-only, so the test receives straight from the (package-internal) rxChan.
func expectFrame(t *testing.T, m *Manager) []byte {
	t.Helper()
	select {
	case frame := <-m.rxChan:
		return frame
	case <-time.After(2 * time.Second):
		t.Fatal("no frame arrived within 2s")
		return nil
	}
}

func TestReaderBoundsFrameLength(t *testing.T) {
	// 1536 garbage bytes with no FEND, then a good frame. Without the cap the
	// reader delivers one 1541-byte mega-frame when the frame's FEND finally
	// arrives. With the cap, only the good frame survives.
	garbage := make([]byte, 3*KISSMaxFrameSize)
	for i := range garbage {
		garbage[i] = 0xAA
	}
	real := Frame([]byte{CMD_DATA, 1, 2, 3})
	m := runReader(t, newFakeStream(garbage, real))
	frame := expectFrame(t, m)
	if len(frame) != 4 || frame[0] != CMD_DATA || frame[1] != 1 {
		t.Fatalf("recovered frame wrong: % x", frame)
	}
}

func TestReaderKeepsLegalMaxLengthFrame(t *testing.T) {
	// A maximum-length payload frame must still pass, including escapes.
	payload := append([]byte{CMD_DATA}, make([]byte, KISSMaxDataSize)...)
	for i := 1; i < len(payload); i++ {
		payload[i] = byte(i * 7) // includes 0xC0 and 0xDB values, exercising escapes
	}
	wire := Frame(payload)
	m := runReader(t, newFakeStream(wire))
	frame := expectFrame(t, m)
	if string(frame) != string(payload) {
		t.Fatalf("max-length frame mangled: got %d bytes, want %d", len(frame), len(payload))
	}
}

func TestReaderDiscardEndsAtFend(t *testing.T) {
	// Overflow triggered (562 bytes > 512 cap), then the frame's FEND gets a
	// chance to end the discard: nothing is forwarded for it (old behaviour
	// forwarded a 564-byte frame full of 0x55), and the follow-up frame still
	// passes intact.
	filler := make([]byte, KISSMaxFrameSize+50)
	for i := range filler {
		filler[i] = 0x55
	}
	var wire []byte
	wire = append(wire, filler...)
	wire = append(wire, Frame([]byte{CMD_DATA, 9})...)
	m := runReader(t, newFakeStream(wire))
	frame := expectFrame(t, m)
	if len(frame) != 2 || frame[0] != CMD_DATA || frame[1] != 9 {
		t.Fatalf("frame after discard is wrong: % x", frame)
	}
}
