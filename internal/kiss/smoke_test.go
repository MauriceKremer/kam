// Hardware smoke test for the KISS modem command layer
// (docs/FeatureKissModem.md §11 and Phase 0). Skipped unless KAM_TEST_PORT
// is set:
//
//	KAM_TEST_PORT=/dev/cu.usbserial-0001 go test ./internal/kiss -run TestHardwareSmoke -v
//
// Optional environment: KAM_TEST_FREQ (Hz, default 869618000), KAM_TEST_BAUD
// (default 115200). The board must not be in use by another reader, and the
// test puts a handful of short frames on the air. Their payloads start with
// 0xFF, a header MeshCore receivers discard, so no node should parse them.
package kiss

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"strconv"
	"testing"
	"time"
)

func envUint32(t *testing.T, name string, fallback uint32) uint32 {
	t.Helper()
	if v := os.Getenv(name); v != "" {
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return uint32(n)
	}
	return fallback
}

func TestHardwareSmoke(t *testing.T) {
	port := os.Getenv("KAM_TEST_PORT")
	if port == "" {
		t.Skip("KAM_TEST_PORT not set; hardware smoke test skipped")
	}
	baud := 115200
	if v := os.Getenv("KAM_TEST_BAUD"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("KAM_TEST_BAUD: %v", err)
		}
		baud = n
	}
	freq := envUint32(t, "KAM_TEST_FREQ", 869618000)
	want := RadioConfig{Frequency: freq, Bandwidth: 62500, SpreadingFactor: 7, CodingRate: 5}

	m, err := NewManager(port, baud)
	if err != nil {
		t.Fatalf("open %s: %v", port, err)
	}
	m.Start()
	defer func() { m.Stop() }()
	client := NewRadioClient(m)
	ctx := context.Background()

	assertAlive := func(stage string) {
		t.Helper()
		select {
		case <-m.Done():
			t.Fatalf("%s: modem link failed: %v", stage, m.Err())
		default:
		}
	}

	// §6.3 startup sequence, steps 1-3 and 7.
	if err := client.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	version, err := client.GetVersion(ctx)
	if err != nil {
		t.Fatalf("GetVersion: %v", err)
	}
	name, err := client.DeviceName(ctx)
	if err != nil {
		t.Fatalf("GetDeviceName: %v", err)
	}
	t.Logf("§6.3 steps 1-2: device=%q version=%+v", name, version)

	if err := client.SetRadio(ctx, want); err != nil {
		t.Fatalf("SetRadio: %v", err)
	}
	echo, err := client.GetRadio(ctx)
	if err != nil {
		t.Fatalf("GetRadio: %v", err)
	}
	if echo != want {
		t.Fatalf("radio echo mismatch: set %+v, read back %+v", want, echo)
	}
	t.Logf("§6.3 steps 3+7: SetRadio accepted and echoed back: %+v", echo)

	// Phase 4 diagnostics captures: log every response, tolerate unsupported.
	probes := []struct {
		label string
		run   func(ctx context.Context) (string, error)
	}{
		{"GetCurrentRSSI", func(c context.Context) (string, error) {
			v, err := client.GetCurrentRSSI(c)
			return strconv.Itoa(int(v)) + " dBm", err
		}},
		{"NoiseFloor", func(c context.Context) (string, error) {
			dbm, calibrated, err := client.NoiseFloor(c)
			return strconv.Itoa(int(dbm)) + " dBm calibrated=" + strconv.FormatBool(calibrated), err
		}},
		{"IsChannelBusy", func(c context.Context) (string, error) {
			v, err := client.IsChannelBusy(c)
			return strconv.FormatBool(v), err
		}},
		{"AirtimeMillis(255)", func(c context.Context) (string, error) {
			v, err := client.AirtimeMillis(c, 255)
			return strconv.FormatUint(uint64(v), 10) + " ms", err
		}},
		{"GetTxPower", func(c context.Context) (string, error) {
			v, err := client.GetTxPower(c)
			return strconv.Itoa(int(v)) + " dBm", err
		}},
		{"GetSignalReport", func(c context.Context) (string, error) {
			v, err := client.GetSignalReport(c)
			return strconv.FormatBool(v), err
		}},
		{"GetStats", func(c context.Context) (string, error) {
			v, err := client.GetStats(c)
			return fmt.Sprintf("%+v", v), err
		}},
		{"BatteryMillivolts", func(c context.Context) (string, error) {
			v, err := client.BatteryMillivolts(c)
			return strconv.Itoa(int(v)) + " mV", err
		}},
		{"MCUTemp", func(c context.Context) (string, error) {
			v, err := client.MCUTempTenthsCelsius(c)
			return strconv.Itoa(int(v)) + " tenths C", err
		}},
		{"GetSensors(3)", func(c context.Context) (string, error) {
			v, err := client.Sensors(c, 3)
			return strconv.Itoa(len(v)) + " bytes", err
		}},
	}
	for _, probe := range probes {
		assertAlive("diagnostics")
		out, err := probe.run(ctx)
		if err != nil {
			t.Logf("diag %-18s -> error: %v", probe.label, err)
		} else {
			t.Logf("diag %-18s -> %s", probe.label, out)
		}
	}

	events := make(chan ModemEvent, 32)
	cancel := m.SubscribeModemEvents(func(e ModemEvent) { events <- e })
	defer cancel()

	drainEvents := func() {
		for {
			select {
			case <-events:
			default:
				return
			}
		}
	}

	// measuredTX sends one data frame through the writer's gate and returns
	// the TxDone latency.
	measuredTX := func(label string, payloadLen int) time.Duration {
		t.Helper()
		assertAlive("tx " + label)
		frame := make([]byte, 1+payloadLen)
		frame[0] = CMD_DATA
		frame[1] = 0xFF
		for i := 2; i < len(frame); i++ {
			frame[i] = 0x55
		}
		drainEvents()
		start := time.Now()
		m.TxChan() <- frame
		for {
			select {
			case event := <-events:
				switch e := event.(type) {
				case TxDone:
					if e.OK {
						latency := time.Since(start)
						t.Logf("%-24s payload=%3dB txdone_ok latency=%s", label, payloadLen, latency.Round(time.Millisecond))
						return latency
					}
					t.Logf("%-24s txdone_ok=false; waiting for writer retry", label)
				case TxBusy:
					t.Logf("%-24s TxBusy; writer will back off and resend", label)
				case TxAbandoned:
					t.Fatalf("%s: frame abandoned by writer", label)
				case ModemFault:
					t.Logf("%-24s modem fault 0x%02X; continuing", label, e.Code)
				}
			case <-time.After(8 * time.Second):
				t.Fatalf("%s: no TxDone within 8s", label)
			}
		}
	}

	samples := func(label string, payloadLen, n int) []time.Duration {
		out := make([]time.Duration, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, measuredTX(label, payloadLen))
		}
		return out
	}

	// R4: TxDone latency at the firmware-default TXDELAY (500 ms).
	lat500a := samples("R4 txdelay=500 base", 8, 3)

	// R3: TXDELAY effect, 0 ms vs 500 ms.
	if err := client.SetTxDelay(ctx, 0); err != nil {
		t.Fatalf("SetTxDelay(0): %v", err)
	}
	lat0 := samples("R3 txdelay=0", 8, 3)
	if err := client.SetTxDelay(ctx, 500); err != nil {
		t.Fatalf("SetTxDelay(500): %v", err)
	}
	lat500b := samples("R4 txdelay=500 recheck", 8, 3)

	// G9/R10: TXtail effect, 0 ms vs 100 ms.
	if err := client.SetTxTail(ctx, 0); err != nil {
		t.Fatalf("SetTxTail(0): %v", err)
	}
	latTail0 := samples("G9 txtail=0", 8, 3)
	if err := client.SetTxTail(ctx, 100); err != nil {
		t.Fatalf("SetTxTail(100): %v", err)
	}
	latTail100 := samples("G9 txtail=100", 8, 3)

	// G5/G3: a second data frame while one is pending should answer
	// Error/TxBusy immediately. Frame 1 goes through the writer gate; frame
	// 2 goes out as a raw request so it reaches the modem while frame 1 is
	// still pending.
	busyObserved := 0
	missingTxDone := 0
	var busyErr error
	for attempt := 1; attempt <= 3; attempt++ {
		assertAlive("busy probe")
		drainEvents()
		longFrame := make([]byte, 1+255)
		longFrame[0] = CMD_DATA
		longFrame[1] = 0xFF
		for i := 2; i < len(longFrame); i++ {
			longFrame[i] = 0x55
		}
		m.TxChan() <- longFrame
		time.Sleep(30 * time.Millisecond) // let the writer write frame 1 first
		_, sendErr := client.exchange(ctx, []byte{CMD_DATA, 0xFF, 0x55, 0x55, 0x55})
		switch {
		case sendErr == nil:
			t.Logf("G5 attempt %d: second frame accepted; frame 1 had already gone (inconclusive)", attempt)
		case errors.Is(sendErr, ErrTxBusy):
			busyObserved++
			busyErr = sendErr
			t.Logf("G5 attempt %d: second frame answered %v (G3/G5 confirmed)", attempt, sendErr)
		case errors.Is(sendErr, context.DeadlineExceeded):
			t.Logf("G5 attempt %d: second frame went out with no reply (frame 1 must have finished early)", attempt)
		default:
			t.Logf("G5 attempt %d: unexpected error: %v", attempt, sendErr)
		}
		_, abandoned := waitTXSettled(t, events, 8*time.Second) // frame 1 (or its resend) must leave the gate
		if abandoned {
			missingTxDone++
			t.Logf("G5 attempt %d: frame 1 TxDone never arrived; the writer released it at the gate timeout", attempt)
		}
	}
	t.Logf("G5/G3 summary: TxBusy observed in %d/3 attempts (last: %v); frame 1 TxDone missing in %d/3 attempts",
		busyObserved, busyErr, missingTxDone)

	reportLatency := func(label string, xs []time.Duration) {
		min, max := xs[0], xs[0]
		for _, x := range xs {
			if x < min {
				min = x
			}
			if x > max {
				max = x
			}
		}
		t.Logf("%s: min=%s max=%s", label, min.Round(time.Millisecond), max.Round(time.Millisecond))
	}
	reportLatency("R4 baseline txdelay=500", lat500a)
	reportLatency("R3 txdelay=0", lat0)
	reportLatency("R4 recheck txdelay=500", lat500b)
	reportLatency("G9 txtail=0", latTail0)
	reportLatency("G9 txtail=100", latTail100)
	t.Logf("counters: txdone_ok=%d txdone_failed=%d tx_busy=%d tx_busy_retry=%d tx_gate_timeout=%d tx_rejected_length=%d rx_unmatched=%d",
		m.counters.txDoneOK.Load(), m.counters.txDoneFailed.Load(), m.counters.txBusy.Load(), m.counters.txBusyRetry.Load(),
		m.counters.txGateTimeout.Load(), m.counters.txRejectedLength.Load(), m.counters.rxUnmatchedFrames.Load())

	// Whether the modem actually transmitted frame 1 after the TxBusy: the
	// GetStats TX counter tells us. Before the busy probe it was 0.
	if stats, err := client.GetStats(ctx); err != nil {
		t.Logf("post-busy GetStats: error: %v", err)
	} else {
		t.Logf("post-busy GetStats: %+v (pre-busy was RX:100 TX:0)", stats)
	}

	// G2: Reboot, then expect an all-zero radio cache (G2) and an
	// uncalibrated noise-floor window (G10). The USB CDC port may briefly
	// drop while the board restarts; reopen the manager if it does.
	if _, err := client.exchange(ctx, []byte{CMD_SETHARDWARE, SUB_REBOOT}, SUB_RESP_OK); err != nil {
		t.Fatalf("Reboot: %v", err)
	}
	t.Log("G2: reboot accepted; waiting for the modem to answer Ping again")
	deadline := time.Now().Add(20 * time.Second)
	back := false
	for time.Now().Before(deadline) {
		select {
		case <-m.Done():
			if client.manager == m { // still the original link
				slog.Warn("smoke test: serial link dropped during reboot; reopening", "err", m.Err())
				m.Stop()
				nm, err := NewManager(port, baud)
				if err != nil {
					t.Fatalf("reopen %s after reboot: %v", port, err)
				}
				nm.Start()
				m = nm
				client = NewRadioClient(m)
			}
		default:
		}
		time.Sleep(500 * time.Millisecond)
		if err := client.Ping(ctx); err == nil {
			back = true
			break
		}
	}
	if !back {
		t.Fatal("G2: modem did not answer Ping within 20s after Reboot")
	}

	zero, err := client.GetRadio(ctx)
	if err != nil {
		t.Fatalf("post-reboot GetRadio: %v", err)
	}
	if !reflect.DeepEqual(zero, RadioConfig{}) {
		t.Errorf("G2: radio cache not zero after reboot: %+v", zero)
	} else {
		t.Log("G2 confirmed: GetRadio returns an all-zero config after reboot")
	}

	if dbm, calibrated, err := client.NoiseFloor(ctx); err != nil {
		t.Logf("G10 NoiseFloor after reboot: error: %v", err)
	} else {
		t.Logf("G10 NoiseFloor after reboot: %d dBm calibrated=%v", dbm, calibrated)
	}
	if rssi, err := client.GetCurrentRSSI(ctx); err != nil {
		t.Logf("post-reboot GetCurrentRSSI: error: %v", err)
	} else {
		t.Logf("post-reboot GetCurrentRSSI: %d dBm", rssi)
	}

	// Leave the board in a working state for production use.
	if err := client.SetRadio(ctx, want); err != nil {
		t.Fatalf("post-reboot SetRadio: %v", err)
	}
	if again, err := client.GetRadio(ctx); err != nil || again != want {
		t.Fatalf("post-reboot echo mismatch: got %+v, err %v", again, err)
	}
	t.Log("radio config re-applied after reboot")
}

func waitTXSettled(t *testing.T, events chan ModemEvent, timeout time.Duration) (done, abandoned bool) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case event := <-events:
			switch e := event.(type) {
			case TxDone:
				if e.OK {
					return true, false
				}
			case TxAbandoned:
				return false, true
			case TxBusy, ModemFault:
				_ = e
			}
		case <-deadline:
			t.Fatalf("modem never settled a pending frame within %s", timeout)
		}
	}
}
