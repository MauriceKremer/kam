package kiss

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"kam.bytecoder.nl/internal/kiss/parsers"
)

// buildGroupTextFrameWithHops assembles a flood-routed GRP_TXT CMD_DATA frame
// carrying the given per-hop path hashes. It is a test-only builder used to
// exercise the router's hop-capture and tap-dispatch paths without a radio.
func buildGroupTextFrameWithHops(t *testing.T, channel, sender, body string, hopStations [][]byte) []byte {
	t.Helper()
	secret, hash, _, ok := parsers.LookupTopic(channel)
	if !ok {
		t.Fatalf("lookup topic %q", channel)
	}

	message := sender + ": " + body
	plainLen := 5 + len(message)
	paddedLen := plainLen
	if r := plainLen % 16; r != 0 {
		paddedLen += 16 - r
	}
	plaintext := make([]byte, paddedLen)
	binary.LittleEndian.PutUint32(plaintext[0:4], uint32(time.Now().Unix()))
	plaintext[4] = 0x00
	copy(plaintext[5:], message)

	macCipher := parsers.EncryptThenMAC(secret, plaintext)
	if macCipher == nil {
		t.Fatal("encrypt then mac failed")
	}

	hashSize := 1
	if len(hopStations) > 0 {
		hashSize = len(hopStations[0])
	}
	pathFlags := byte((hashSize-1)&0x03)<<6 | byte(len(hopStations)&0x3F)

	raw := []byte{
		(PAYLOAD_VERSION_V1 << 6) | (PAYLOAD_TYPE_GRP_TXT << 2) | ROUTE_TYPE_FLOOD,
		pathFlags,
	}
	for _, h := range hopStations {
		raw = append(raw, h...)
	}
	raw = append(raw, hash)
	raw = append(raw, macCipher...)

	return append([]byte{CMD_DATA}, raw...)
}

func TestRouterCapturesHopStationsAndDispatchesTap(t *testing.T) {
	parsers.ResetTopicRegistry()
	if err := parsers.SetPublicChannelSecretHex("8b3387e9c5cdea6ac9e5edbaa115cd72"); err != nil {
		t.Fatalf("set public secret: %v", err)
	}

	m := &Manager{}
	m.TestInit()
	m.rxMetaTimeout = 20 * time.Millisecond

	var got parsers.GroupTextMessage
	ready := make(chan struct{})
	m.SubscribeGroupText(func(msg parsers.GroupTextMessage) {
		got = msg
		close(ready)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.routeLoop(ctx)
	defer func() { cancel(); time.Sleep(50 * time.Millisecond) }()

	hopStations := [][]byte{{0xAA}, {0xBB}}
	frame := buildGroupTextFrameWithHops(t, "#public", "kam", "hi", hopStations)

	select {
	case m.TestRxChan() <- frame:
	case <-time.After(time.Second):
		t.Fatal("timeout pushing frame to rxChan")
	}

	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for group text tap")
	}

	if got.HopCount != 2 {
		t.Fatalf("HopCount: got %d, want 2", got.HopCount)
	}
	if len(got.HopStations) != 2 {
		t.Fatalf("HopStations len: got %d, want 2", len(got.HopStations))
	}
	if got.HopStations[0][0] != 0xAA || got.HopStations[1][0] != 0xBB {
		t.Fatalf("HopStations: got %x, want [[aa],[bb]]", got.HopStations)
	}
	if got.Sender != "kam" {
		t.Fatalf("Sender: got %q, want %q", got.Sender, "kam")
	}
	if got.Body != "hi" {
		t.Fatalf("Body: got %q, want %q", got.Body, "hi")
	}
	if got.ChannelName != "#public" {
		t.Fatalf("ChannelName: got %q, want #public", got.ChannelName)
	}
}

func TestRouterTwoByteHashHopStations(t *testing.T) {
	parsers.ResetTopicRegistry()
	if err := parsers.SetPublicChannelSecretHex("8b3387e9c5cdea6ac9e5edbaa115cd72"); err != nil {
		t.Fatalf("set public secret: %v", err)
	}

	m := &Manager{}
	m.TestInit()
	m.rxMetaTimeout = 20 * time.Millisecond

	var got parsers.GroupTextMessage
	ready := make(chan struct{})
	m.SubscribeGroupText(func(msg parsers.GroupTextMessage) {
		got = msg
		close(ready)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.routeLoop(ctx)
	defer func() { cancel(); time.Sleep(50 * time.Millisecond) }()

	hopStations := [][]byte{{0xAA, 0xBB}, {0xCC, 0xDD}}
	frame := buildGroupTextFrameWithHops(t, "#public", "alice", "yo", hopStations)

	select {
	case m.TestRxChan() <- frame:
	case <-time.After(time.Second):
		t.Fatal("timeout pushing frame to rxChan")
	}

	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for group text tap")
	}

	if got.HopCount != 2 {
		t.Fatalf("HopCount: got %d, want 2", got.HopCount)
	}
	if len(got.HopStations) != 2 || len(got.HopStations[0]) != 2 {
		t.Fatalf("HopStations: got %x, want 2x2-byte hashes", got.HopStations)
	}
	if got.HopStations[0][0] != 0xAA || got.HopStations[1][1] != 0xDD {
		t.Fatalf("HopStations values: got %x", got.HopStations)
	}
}

func TestRouterDispatchesTapWithoutStore(t *testing.T) {
	parsers.ResetTopicRegistry()
	if err := parsers.SetPublicChannelSecretHex("8b3387e9c5cdea6ac9e5edbaa115cd72"); err != nil {
		t.Fatalf("set public secret: %v", err)
	}

	m := &Manager{}
	m.TestInit()
	m.rxMetaTimeout = 20 * time.Millisecond
	// m.Store is intentionally nil; tap must still fire.

	var got parsers.GroupTextMessage
	ready := make(chan struct{})
	m.SubscribeGroupText(func(msg parsers.GroupTextMessage) {
		got = msg
		close(ready)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.routeLoop(ctx)
	defer func() { cancel(); time.Sleep(50 * time.Millisecond) }()

	frame := buildGroupTextFrameWithHops(t, "#public", "kam", "no store", nil)
	select {
	case m.TestRxChan() <- frame:
	case <-time.After(time.Second):
		t.Fatal("timeout pushing frame to rxChan")
	}

	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for tap with nil Store")
	}

	if got.HopCount != 0 || len(got.HopStations) != 0 {
		t.Fatalf("expected zero hops, got count=%d stations=%x", got.HopCount, got.HopStations)
	}
}

// buildRxMetaFrame builds a SetHardware RxMeta (0xF9) frame carrying the given
// SNR (quarter-dB units) and RSSI (dBm) signed values.
func buildRxMetaFrame(snr, rssi int) []byte {
	return []byte{CMD_SETHARDWARE, SUB_RX_META, byte(int8(snr)), byte(int8(rssi))}
}

func receiveControlFrame(t *testing.T, m *Manager) []byte {
	t.Helper()
	select {
	case control := <-m.TestControlChan():
		if control.done != nil {
			control.done <- nil
		}
		return control.frame
	case <-time.After(time.Second):
		t.Fatal("control frame was not sent")
		return nil
	}
}

func TestRouterAttachesRxMetaToGroupText(t *testing.T) {
	parsers.ResetTopicRegistry()
	if err := parsers.SetPublicChannelSecretHex("8b3387e9c5cdea6ac9e5edbaa115cd72"); err != nil {
		t.Fatalf("set public secret: %v", err)
	}

	m := &Manager{}
	m.TestInit()
	m.rxMetaTimeout = time.Second // rely on RxMeta, not the fallback timer

	var got parsers.GroupTextMessage
	ready := make(chan struct{})
	m.SubscribeGroupText(func(msg parsers.GroupTextMessage) {
		got = msg
		close(ready)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.routeLoop(ctx)
	defer func() { cancel(); time.Sleep(50 * time.Millisecond) }()

	data := buildGroupTextFrameWithHops(t, "#public", "alice", "@kam ping", [][]byte{{0xAA}})
	select {
	case m.TestRxChan() <- data:
	case <-time.After(time.Second):
		t.Fatal("timeout pushing data frame")
	}
	// RxMeta follows the data frame per the KISS protocol.
	select {
	case m.TestRxChan() <- buildRxMetaFrame(29, -64): // SNR 7.25 dB, RSSI -64 dBm
	case <-time.After(time.Second):
		t.Fatal("timeout pushing rxmeta frame")
	}

	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for tap after rxmeta")
	}

	if !got.HasRxMeta {
		t.Fatal("HasRxMeta should be true")
	}
	if got.SNR != 29 {
		t.Fatalf("SNR: got %d, want 29", got.SNR)
	}
	if got.RSSI != -64 {
		t.Fatalf("RSSI: got %d, want -64", got.RSSI)
	}
	if got.HopCount != 1 || len(got.HopStations) != 1 || got.HopStations[0][0] != 0xAA {
		t.Fatalf("hop info lost: count=%d stations=%x", got.HopCount, got.HopStations)
	}
	if got.Sender != "alice" {
		t.Fatalf("sender: got %q", got.Sender)
	}
}

func TestRouterRxMetaTimeoutFallback(t *testing.T) {
	parsers.ResetTopicRegistry()
	if err := parsers.SetPublicChannelSecretHex("8b3387e9c5cdea6ac9e5edbaa115cd72"); err != nil {
		t.Fatalf("set public secret: %v", err)
	}

	m := &Manager{}
	m.TestInit()
	m.rxMetaTimeout = 20 * time.Millisecond

	var got parsers.GroupTextMessage
	ready := make(chan struct{})
	m.SubscribeGroupText(func(msg parsers.GroupTextMessage) {
		got = msg
		close(ready)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.routeLoop(ctx)
	defer func() { cancel(); time.Sleep(50 * time.Millisecond) }()

	data := buildGroupTextFrameWithHops(t, "#public", "alice", "no meta", [][]byte{{0xBB}})
	select {
	case m.TestRxChan() <- data:
	case <-time.After(time.Second):
		t.Fatal("timeout pushing data frame")
	}

	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for fallback dispatch")
	}

	if got.HasRxMeta {
		t.Fatal("HasRxMeta should be false on timeout fallback")
	}
	if got.HopCount != 1 || got.Sender != "alice" {
		t.Fatalf("fallback lost hop/sender: count=%d sender=%q", got.HopCount, got.Sender)
	}
}

func TestRouterRxMetaWithoutPendingIgnored(t *testing.T) {
	parsers.ResetTopicRegistry()
	if err := parsers.SetPublicChannelSecretHex("8b3387e9c5cdea6ac9e5edbaa115cd72"); err != nil {
		t.Fatalf("set public secret: %v", err)
	}

	m := &Manager{}
	m.TestInit()
	m.rxMetaTimeout = 20 * time.Millisecond

	fired := make(chan struct{})
	m.SubscribeGroupText(func(msg parsers.GroupTextMessage) { close(fired) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.routeLoop(ctx)
	defer func() { cancel(); time.Sleep(50 * time.Millisecond) }()

	select {
	case m.TestRxChan() <- buildRxMetaFrame(10, -70):
	case <-time.After(time.Second):
		t.Fatal("timeout pushing rxmeta")
	}

	select {
	case <-fired:
		t.Fatal("tap must not fire for orphan RxMeta")
	case <-time.After(100 * time.Millisecond):
		// expected: no dispatch
	}
}

func TestRadioClientReturnsModemErrorImmediately(t *testing.T) {
	m := &Manager{}
	m.TestInit()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.routeLoop(ctx)

	commandCtx, commandCancel := context.WithTimeout(context.Background(), time.Second)
	defer commandCancel()
	result := make(chan error, 1)
	go func() {
		result <- NewRadioClient(m).SetRadio(commandCtx, RadioConfig{
			Frequency: 869618000, Bandwidth: 62500, SpreadingFactor: 7, CodingRate: 5,
		})
	}()

	receiveControlFrame(t, m)
	select {
	case m.TestRxChan() <- []byte{CMD_SETHARDWARE, SUB_ERROR, 0x01}:
	case <-time.After(time.Second):
		t.Fatal("modem error was not routed")
	}

	select {
	case err := <-result:
		if !errors.Is(err, ErrInvalidLength) {
			t.Fatalf("SetRadio error = %v, want ErrInvalidLength", err)
		}
		var modemErr *ModemError
		if !errors.As(err, &modemErr) || modemErr.Code != 0x01 {
			t.Fatalf("SetRadio error = %#v, want typed code 0x01", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("SetRadio did not return the modem error promptly")
	}
}

func TestRadioClientUsesPingBarrierAfterTimeout(t *testing.T) {
	m := &Manager{}
	m.TestInit()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.routeLoop(ctx)
	radio := NewRadioClient(m)

	timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer timeoutCancel()
	result := make(chan error, 1)
	go func() {
		result <- radio.SetRadio(timeoutCtx, RadioConfig{
			Frequency: 869618000, Bandwidth: 62500, SpreadingFactor: 7, CodingRate: 5,
		})
	}()
	frame := receiveControlFrame(t, m)
	if len(frame) < 2 || frame[1] != SUB_SET_RADIO {
		t.Fatalf("initial command = %x, want SetRadio", frame)
	}
	if err := <-result; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SetRadio error = %v, want deadline exceeded", err)
	}

	go func() {
		_, err := radio.GetRadio(context.Background())
		result <- err
	}()

	frame = receiveControlFrame(t, m)
	if len(frame) != 2 || frame[0] != CMD_SETHARDWARE || frame[1] != SUB_PING {
		t.Fatalf("first resync frame = %x, want SetHardware/Ping", frame)
	}
	select {
	case m.TestRxChan() <- []byte{CMD_SETHARDWARE, SUB_RESP_OK}:
	case <-time.After(time.Second):
		t.Fatal("stale response was not injected")
	}
	select {
	case control := <-m.TestControlChan():
		t.Fatalf("next request sent before Pong: %x", control.frame)
	case <-time.After(25 * time.Millisecond):
	}
	select {
	case m.TestRxChan() <- []byte{CMD_SETHARDWARE, SUB_RESP_PONG}:
	case <-time.After(time.Second):
		t.Fatal("Pong was not injected")
	}
	frame = receiveControlFrame(t, m)
	if len(frame) != 2 || frame[1] != SUB_GET_RADIO {
		t.Fatalf("post-barrier request = %x, want GetRadio", frame)
	}
	select {
	case m.TestRxChan() <- []byte{CMD_SETHARDWARE, SUB_RESP_RADIO, 0x50, 0x51, 0xD5, 0x33, 0x24, 0xF4, 0, 0, 7, 5}:
	case <-time.After(time.Second):
		t.Fatal("GetRadio response was not injected")
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("GetRadio after barrier: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("GetRadio did not finish after barrier")
	}
}

func TestRouterPublishesTxDoneAndTxBusy(t *testing.T) {
	m := &Manager{}
	m.TestInit()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.routeLoop(ctx)

	m.TestRxChan() <- []byte{CMD_SETHARDWARE, SUB_TX_DONE, 1}
	select {
	case event := <-m.txGateEvents:
		if !event.done {
			t.Fatalf("TxDone gate event = %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("TxDone did not reach TX gate")
	}
	if event := <-m.eventChan; event.(TxDone).OK != true {
		t.Fatalf("modem TxDone event = %#v", event)
	}

	m.TestRxChan() <- []byte{CMD_SETHARDWARE, SUB_ERROR, MODEM_ERROR_TX_BUSY}
	select {
	case event := <-m.txGateEvents:
		if !event.busy {
			t.Fatalf("TxBusy gate event = %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("TxBusy did not reach TX gate")
	}
	if _, ok := (<-m.eventChan).(TxBusy); !ok {
		t.Fatal("modem TxBusy event was not published")
	}
	counters := m.Counters()
	if counters.TxDoneOK != 1 || counters.TxBusy != 1 {
		t.Fatalf("modem counters = %+v", counters)
	}
}
