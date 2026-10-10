package kiss

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestEncodeSetRadioGoldenFrame(t *testing.T) {
	got := encodeSetRadioRequest(RadioConfig{
		Frequency: 869618000, Bandwidth: 62500, SpreadingFactor: 7, CodingRate: 5,
	})
	want := []byte{0x06, 0x09, 0x50, 0x51, 0xD5, 0x33, 0x24, 0xF4, 0x00, 0x00, 0x07, 0x05}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SetRadio frame = % X, want % X", got, want)
	}
}

func TestFrameEscapesRxMetaRSSIMinus64(t *testing.T) {
	got := Frame([]byte{CMD_SETHARDWARE, SUB_RX_META, 29, 0xC0})
	want := []byte{FEND, CMD_SETHARDWARE, SUB_RX_META, 29, FESC, TFEND, FEND}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("framed RxMeta = % X, want % X", got, want)
	}
}

func TestDecodeRadioResponseRejectsShortPayload(t *testing.T) {
	if _, err := decodeRadioResponse([]byte{1, 2, 3}); !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("decodeRadioResponse error = %v, want ErrMalformedResponse", err)
	}
}

func TestProtocolResponseDecoders(t *testing.T) {
	version, err := decodeVersionResponse([]byte{1, 0})
	if err != nil || version != (FirmwareVersion{Version: 1}) {
		t.Fatalf("version decode = %+v, %v", version, err)
	}
	stats, err := decodeStatsResponse([]byte{1, 0, 0, 0, 2, 0, 0, 0, 3, 0, 0, 0})
	if err != nil || stats != (ModemStats{RX: 1, TX: 2, Errors: 3}) {
		t.Fatalf("stats decode = %+v, %v", stats, err)
	}
	if _, err := decodeBoolResponse([]byte{2}, "test"); !errors.Is(err, ErrMalformedResponse) {
		t.Fatalf("invalid boolean error = %v", err)
	}
	if got := decodeSensorsResponse(nil); got != nil {
		t.Fatalf("empty sensor payload = %v, want nil", got)
	}
}

func TestSetRadioValidatesBeforeEnqueue(t *testing.T) {
	m := &Manager{}
	m.TestInit()
	err := NewRadioClient(m).SetRadio(context.Background(), RadioConfig{
		Frequency: 869618000, Bandwidth: 62500, SpreadingFactor: 4, CodingRate: 5,
	})
	if err == nil {
		t.Fatal("invalid spreading factor was accepted")
	}
	select {
	case frame := <-m.TestControlChan():
		t.Fatalf("invalid settings queued a frame: %x", frame.frame)
	default:
	}
}

func TestUnsupportedModemErrorsMatchSentinel(t *testing.T) {
	for _, code := range []byte{MODEM_ERROR_NO_CALLBACK, MODEM_ERROR_UNKNOWN_CMD} {
		if !errors.Is(&ModemError{Code: code}, ErrUnsupported) {
			t.Fatalf("modem error 0x%02X did not match ErrUnsupported", code)
		}
	}
}

func TestCommandContextAppliesPerRequestTimeout(t *testing.T) {
	parent, cancelParent := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelParent()
	child, cancelChild := commandContext(parent)
	defer cancelChild()
	deadline, ok := child.Deadline()
	if !ok || time.Until(deadline) > defaultCommandTimeout+100*time.Millisecond {
		t.Fatalf("command deadline = %v, want at most %v from now", deadline, defaultCommandTimeout)
	}
}

func TestSetCSMAWritesParametersThenPingBarrier(t *testing.T) {
	m := &Manager{}
	m.TestInit()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.routeLoop(ctx)
	radio := NewRadioClient(m)
	result := make(chan error, 1)
	go func() {
		result <- radio.SetCSMA(context.Background(), CSMAConfig{
			TxDelayMS: 500, Persistence: 63, SlotTimeMS: 100,
		})
	}()

	want := [][]byte{{CMD_TXDELAY, 50}, {CMD_PERSISTENCE, 63}, {CMD_SLOTTIME, 10}, {CMD_FULLDUPLEX, 0}}
	for _, frame := range want {
		if got := receiveControlFrame(t, m); !reflect.DeepEqual(got, frame) {
			t.Fatalf("CSMA frame = % X, want % X", got, frame)
		}
	}
	if got := receiveControlFrame(t, m); !reflect.DeepEqual(got, []byte{CMD_SETHARDWARE, SUB_PING}) {
		t.Fatalf("CSMA barrier = % X, want SetHardware/Ping", got)
	}
	m.TestRxChan() <- []byte{CMD_SETHARDWARE, SUB_RESP_PONG}
	if err := <-result; err != nil {
		t.Fatalf("SetCSMA: %v", err)
	}
}
