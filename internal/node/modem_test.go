package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kam.bytecoder.nl/internal/api"
	"kam.bytecoder.nl/internal/kiss"
	"kam.bytecoder.nl/internal/models"
)

// fakeModemRadio scripts a KISS modem: values the calls read and write, per
// method error injection, and a hook the file-conflict test uses to mutate
// kam.yml mid-change.
type fakeModemRadio struct {
	mu         sync.Mutex
	echo       kiss.RadioConfig
	mismatch   bool // the next GetRadio answers a bumped frequency
	power      uint8
	signal     bool
	signalFail bool // GetSignalReport answers (false, nil)
	rssi       int8
	busy       bool
	floor      int16
	stats      kiss.ModemStats
	battery    uint16
	tempTenths int16
	sensors    []byte
	version    kiss.FirmwareVersion
	deviceName string
	airtime    uint32

	errs     map[string]error // method name → that method fails always
	failFrom map[string]int   // method name → fails (with a timeout) from call n on
	calls    []string
	counts   map[string]int
	onSet    func() // runs after a successful SetRadio
}

func newFakeRadio() *fakeModemRadio {
	return &fakeModemRadio{
		echo:       kiss.RadioConfig{Frequency: 869618000, Bandwidth: 62500, SpreadingFactor: 7, CodingRate: 5},
		version:    kiss.FirmwareVersion{Version: 1},
		errs:       map[string]error{},
		failFrom:   map[string]int{},
		counts:     map[string]int{},
		stats:      kiss.ModemStats{RX: 10, TX: 2, Errors: 0},
		deviceName: "Emmen KISS",
	}
}

func (f *fakeModemRadio) fault(name string) error {
	f.counts[name]++
	if err := f.errs[name]; err != nil {
		return err
	}
	if n, ok := f.failFrom[name]; ok && f.counts[name] >= n {
		return context.DeadlineExceeded
	}
	return nil
}

func (f *fakeModemRadio) noted(name string) error {
	f.calls = append(f.calls, name)
	return f.fault(name)
}

func (f *fakeModemRadio) Ping(ctx context.Context) error { return f.noted("Ping") }

func (f *fakeModemRadio) GetRadio(ctx context.Context) (kiss.RadioConfig, error) {
	f.calls = append(f.calls, "GetRadio")
	if err := f.fault("GetRadio"); err != nil {
		return kiss.RadioConfig{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mismatch {
		f.mismatch = false
		return kiss.RadioConfig{Frequency: f.echo.Frequency + 1, Bandwidth: f.echo.Bandwidth, SpreadingFactor: f.echo.SpreadingFactor, CodingRate: f.echo.CodingRate}, nil
	}
	return f.echo, nil
}

func (f *fakeModemRadio) SetRadio(ctx context.Context, config kiss.RadioConfig) error {
	if err := f.noted("SetRadio"); err != nil {
		return err
	}
	f.mu.Lock()
	f.echo = config
	hook, onSet := f.onSet, f.onSet
	if hook != nil {
		f.onSet = nil
	}
	f.mu.Unlock()
	if onSet != nil {
		onSet()
	}
	return nil
}

func (f *fakeModemRadio) GetTxPower(ctx context.Context) (uint8, error) {
	if err := f.noted("GetTxPower"); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.power, nil
}

func (f *fakeModemRadio) SetTxPower(ctx context.Context, dbm uint8) error {
	if err := f.noted("SetTxPower"); err != nil {
		return err
	}
	f.mu.Lock()
	f.power = dbm
	f.mu.Unlock()
	return nil
}

func (f *fakeModemRadio) GetSignalReport(ctx context.Context) (bool, error) {
	if err := f.noted("GetSignalReport"); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.signalFail {
		return false, nil
	}
	return f.signal, nil
}

func (f *fakeModemRadio) SetSignalReport(ctx context.Context, enabled bool) (bool, error) {
	if err := f.noted("SetSignalReport"); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.signalFail {
		return false, nil
	}
	f.signal = enabled
	return enabled, nil
}

func (f *fakeModemRadio) SetTxDelay(ctx context.Context, ms int) error { return f.noted("SetTxDelay") }
func (f *fakeModemRadio) SetPersistence(ctx context.Context, v uint8) error {
	return f.noted("SetPersistence")
}
func (f *fakeModemRadio) SetSlotTime(ctx context.Context, ms int) error {
	return f.noted("SetSlotTime")
}
func (f *fakeModemRadio) SetFullDuplex(ctx context.Context, v bool) error {
	return f.noted("SetFullDuplex")
}

func (f *fakeModemRadio) GetCurrentRSSI(ctx context.Context) (int8, error) {
	if err := f.noted("GetCurrentRSSI"); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rssi, nil
}

func (f *fakeModemRadio) IsChannelBusy(ctx context.Context) (bool, error) {
	if err := f.noted("IsChannelBusy"); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.busy, nil
}

func (f *fakeModemRadio) AirtimeMillis(ctx context.Context, length uint8) (uint32, error) {
	if err := f.noted("AirtimeMillis"); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.airtime, nil
}

func (f *fakeModemRadio) NoiseFloor(ctx context.Context) (int16, bool, error) {
	if err := f.noted("NoiseFloor"); err != nil {
		return 0, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.floor, f.floor != 0, nil
}

func (f *fakeModemRadio) GetVersion(ctx context.Context) (kiss.FirmwareVersion, error) {
	if err := f.noted("GetVersion"); err != nil {
		return kiss.FirmwareVersion{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.version, nil
}

func (f *fakeModemRadio) GetStats(ctx context.Context) (kiss.ModemStats, error) {
	if err := f.noted("GetStats"); err != nil {
		return kiss.ModemStats{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stats, nil
}

func (f *fakeModemRadio) BatteryMillivolts(ctx context.Context) (uint16, error) {
	if err := f.noted("BatteryMillivolts"); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.battery, nil
}

func (f *fakeModemRadio) MCUTempTenthsCelsius(ctx context.Context) (int16, error) {
	if err := f.noted("MCUTempTenthsCelsius"); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tempTenths, nil
}

func (f *fakeModemRadio) Sensors(ctx context.Context, permissions uint8) ([]byte, error) {
	if err := f.noted("Sensors"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.sensors...), nil
}

func (f *fakeModemRadio) DeviceName(ctx context.Context) (string, error) {
	if err := f.noted("DeviceName"); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deviceName, nil
}

func (f *fakeModemRadio) CachedIdentity() (*string, *uint8) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var name *string
	var version *uint8
	if f.deviceName != "" {
		value := f.deviceName
		name = &value
	}
	if f.version.Version != 0 {
		value := f.version.Version
		version = &value
	}
	return name, version
}

// fakeScheduler records the pause/resume coordination and can hold AwaitIdle
// until its context runs out (an always-in-flight frame).
type fakeScheduler struct {
	mu       sync.Mutex
	paused   bool
	inFlight bool
	pauses   int
	resumes  int
	awaits   int
}

func (f *fakeScheduler) Pause() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused = true
	f.pauses++
}

func (f *fakeScheduler) Resume() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused = false
	f.resumes++
}

func (f *fakeScheduler) AwaitIdle(ctx context.Context) error {
	f.mu.Lock()
	f.awaits++
	inFlight := f.inFlight
	f.mu.Unlock()
	if inFlight {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (f *fakeScheduler) pauseState() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paused
}

var modemTestYAML = `version: 1
node_name: test
db_path: kam.db
radio:
  port: /dev/ttyUSB0
  baud: 115200
  frequency_hz: 869618000
  bandwidth_hz: 62500
  spreading_factor: 7
  coding_rate: 5
channels:
  - name: "#ops"
    key_strategy: derived
    enabled: true
`

func modemTestConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kam.yml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func modemTestController(radio *fakeModemRadio, scheduler *fakeScheduler, configPath string) *modemController {
	manager := &kiss.Manager{}
	manager.TestInit()
	runtime := &radioRuntime{
		settings: models.RadioConfig{Port: "/dev/ttyUSB0", Baud: 115200, FrequencyHz: 869618000, BandwidthHz: 62500, SpreadingFactor: 7, CodingRate: 5},
		echo:     &kiss.RadioConfig{Frequency: 869618000, Bandwidth: 62500, SpreadingFactor: 7, CodingRate: 5},
	}
	return newModemController(radio, scheduler, manager, runtime, configPath, nil)
}

func modemClass(t *testing.T, err error) *api.ModemError {
	t.Helper()
	var modemErr *api.ModemError
	if !errors.As(err, &modemErr) {
		t.Fatalf("error %v is not an api.ModemError", err)
	}
	return modemErr
}

var validRadioRequest = api.ModemRadioRequest{
	FrequencyHz:     ptrFloat64(869618000),
	BandwidthHz:     ptrFloat64(62500),
	SpreadingFactor: ptrFloat64(7),
	CodingRate:      ptrFloat64(5),
}

func ptrFloat64(value float64) *float64 { return &value }
func ptrString(value string) *string    { return &value }
func ptrBool(value bool) *bool          { return &value }

// --- Validation -------------------------------------------------------------

func TestModemDecodeRadioRequestValidation(t *testing.T) {
	cases := []struct {
		name    string
		request func(*api.ModemRadioRequest)
		code    api.ModemErrorClass
		message string
	}{
		{"missing frequency", func(r *api.ModemRadioRequest) { r.FrequencyHz = nil }, api.ModemErrorInvalid, "frequency_hz is required"},
		{"missing coding rate", func(r *api.ModemRadioRequest) { r.CodingRate = nil }, api.ModemErrorInvalid, "coding_rate is required"},
		{"fractional spreading factor", func(r *api.ModemRadioRequest) { r.SpreadingFactor = ptrFloat64(7.5) }, api.ModemErrorInvalid, "spreading_factor"},
		{"spreading factor above uint8", func(r *api.ModemRadioRequest) { r.SpreadingFactor = ptrFloat64(300) }, api.ModemErrorInvalid, "spreading_factor"},
		{"negative bandwidth", func(r *api.ModemRadioRequest) { r.BandwidthHz = ptrFloat64(-1) }, api.ModemErrorInvalid, "bandwidth_hz"},
		{"spreading factor out of lora range", func(r *api.ModemRadioRequest) { r.SpreadingFactor = ptrFloat64(13) }, api.ModemErrorInvalid, "radio.spreading_factor"},
		{"tx power out of range", func(r *api.ModemRadioRequest) { r.TxPowerDBm = ptrFloat64(40) }, api.ModemErrorInvalid, "tx_power_dbm"},
		{"incomplete csma", func(r *api.ModemRadioRequest) { r.CSMA = &api.ModemRadioCSMA{TxDelayMS: ptrFloat64(500)} }, api.ModemErrorInvalid, "csma requires"},
		{"csma not a multiple of 10", func(r *api.ModemRadioRequest) {
			r.CSMA = &api.ModemRadioCSMA{TxDelayMS: ptrFloat64(505), Persistence: ptrFloat64(63), SlotTimeMS: ptrFloat64(100), FullDuplex: ptrBool(false)}
		}, api.ModemErrorInvalid, "csma.txdelay_ms"},
		{"fractional tx power", func(r *api.ModemRadioRequest) { r.TxPowerDBm = ptrFloat64(14.5) }, api.ModemErrorInvalid, "tx_power_dbm"},
	}
	for _, want := range cases {
		request := validRadioRequest
		want.request(&request)
		_, _, _, err := decodeRadioRequest(request)
		if err == nil {
			t.Fatalf("%s: accepted", want.name)
		}
		class := modemClass(t, err)
		if class.Class != want.code || !strings.Contains(class.Message, want.message) {
			t.Fatalf("%s: %d %q", want.name, class.Class, class.Message)
		}
	}
	if _, _, _, err := decodeRadioRequest(validRadioRequest); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
}

// --- Query ------------------------------------------------------------------

func TestModemQueryValidationAndDispatch(t *testing.T) {
	controller := modemTestController(newFakeRadio(), &fakeScheduler{}, "")
	cases := []struct {
		name    string
		request api.ModemQueryRequest
		code    api.ModemErrorClass
		message string
	}{
		{"empty command", api.ModemQueryRequest{}, api.ModemErrorBadRequest, "command is required"},
		{"unknown command", api.ModemQueryRequest{Command: "SetRadio"}, api.ModemErrorBadRequest, "unknown command"},
		{"missing length", api.ModemQueryRequest{Command: "GetAirtime"}, api.ModemErrorInvalid, "length is required for GetAirtime"},
		{"fractional length", api.ModemQueryRequest{Command: "GetAirtime", Length: ptrFloat64(12.5)}, api.ModemErrorInvalid, "length must be an integer"},
		{"zero length", api.ModemQueryRequest{Command: "GetAirtime", Length: ptrFloat64(0)}, api.ModemErrorInvalid, "length"},
		{"length too big", api.ModemQueryRequest{Command: "GetAirtime", Length: ptrFloat64(256)}, api.ModemErrorInvalid, "length"},
		{"stray length", api.ModemQueryRequest{Command: "Ping", Length: ptrFloat64(5)}, api.ModemErrorInvalid, "does not take length"},
		{"missing permissions", api.ModemQueryRequest{Command: "GetSensors"}, api.ModemErrorInvalid, "permissions is required for GetSensors"},
		{"permissions out of range", api.ModemQueryRequest{Command: "GetSensors", Permissions: ptrFloat64(8)}, api.ModemErrorInvalid, "permissions"},
		{"stray permissions", api.ModemQueryRequest{Command: "GetStats", Permissions: ptrFloat64(3)}, api.ModemErrorInvalid, "does not take permissions"},
	}
	for _, want := range cases {
		_, err := controller.Query(context.Background(), want.request)
		if err == nil {
			t.Fatalf("%s: accepted", want.name)
		}
		class := modemClass(t, err)
		if class.Class != want.code || !strings.Contains(class.Message, want.message) {
			t.Fatalf("%s: got %d %q", want.name, class.Class, class.Message)
		}
	}

	// The valid argumented commands.
	if _, err := controller.Query(context.Background(), api.ModemQueryRequest{Command: "GetAirtime", Length: ptrFloat64(255)}); err != nil {
		t.Fatalf("GetAirtime 255: %v", err)
	}
	if _, err := controller.Query(context.Background(), api.ModemQueryRequest{Command: "GetSensors", Permissions: ptrFloat64(7)}); err != nil {
		t.Fatalf("GetSensors 7: %v", err)
	}
}

func TestModemQueryTypedValues(t *testing.T) {
	radio := newFakeRadio()
	radio.rssi = -106
	radio.busy = true
	radio.floor = -114
	radio.power = 14
	radio.tempTenths = 324
	radio.battery = 3900
	radio.sensors = []byte{1, 2, 3}
	radio.airtime = 530
	radio.signal = true
	radio.stats = kiss.ModemStats{RX: 1246, TX: 38, Errors: 0}
	controller := modemTestController(radio, &fakeScheduler{}, "")
	fourteen := uint8(14)
	controller.txPowerEcho = &fourteen

	queries := []struct {
		name  string
		want  any
		match func(value any) bool
	}{
		{"Ping", "pong", func(v any) bool { return v == "pong" }},
		{"GetRadio", nil, func(v any) bool {
			tuple, ok := v.(api.ModemRadioValue)
			return ok && tuple == api.ModemRadioValue{FrequencyHz: 869618000, BandwidthHz: 62500, SpreadingFactor: 7, CodingRate: 5}
		}},
		{"GetTxPower", nil, func(v any) bool { return v == 14 }},
		{"GetCurrentRssi", nil, func(v any) bool { return v == -106 }},
		{"IsChannelBusy", nil, func(v any) bool { return v == true }},
		{"GetAirtime", nil, func(v any) bool { return v == int64(530) }},
		{"GetNoiseFloor", nil, func(v any) bool { return v == -114 }},
		{"GetVersion", nil, func(v any) bool {
			version, ok := v.(api.ModemVersionValue)
			return ok && version == api.ModemVersionValue{Version: 1}
		}},
		{"GetStats", nil, func(v any) bool {
			stats, ok := v.(api.ModemStats)
			return ok && stats == api.ModemStats{RX: 1246, TX: 38}
		}},
		{"GetBattery", nil, func(v any) bool { return v == 3900 }},
		{"GetMCUTemp", nil, func(v any) bool {
			return v == 32.4
		}},
		{"GetDeviceName", nil, func(v any) bool { return v == "Emmen KISS" }},
		{"GetSignalReport", nil, func(v any) bool { return v == true }},
		{"GetSensors", nil, func(v any) bool {
			sensors, ok := v.(api.ModemSensorsValue)
			return ok && sensors == api.ModemSensorsValue{Permissions: 3, DataHex: "010203"}
		}},
	}
	for _, want := range queries {
		request := api.ModemQueryRequest{Command: want.name}
		switch want.name {
		case "GetAirtime":
			request.Length = ptrFloat64(255)
		case "GetSensors":
			request.Permissions = ptrFloat64(3)
		}
		result, err := controller.Query(context.Background(), request)
		if err != nil {
			t.Fatalf("%s: %v", want.name, err)
		}
		if result.Command != want.name || !result.Supported || result.Value == nil {
			t.Fatalf("%s: %+v", want.name, result)
		}
		if result.DurationMS < 0 {
			t.Fatalf("%s: duration %d", want.name, result.DurationMS)
		}
		if want.match != nil && !want.match(result.Value) {
			t.Fatalf("%s: value %#v", want.name, result.Value)
		}
	}
}

// Unsupported boards are normal responses, not errors (API-MD-10).
func TestModemQueryUnsupported(t *testing.T) {
	radio := newFakeRadio()
	radio.errs["BatteryMillivolts"] = &kiss.ModemError{Code: 0x03} // NO_CALLBACK
	controller := modemTestController(radio, &fakeScheduler{}, "")
	result, err := controller.Query(context.Background(), api.ModemQueryRequest{Command: "GetBattery"})
	if err != nil {
		t.Fatalf("GetBattery: %v", err)
	}
	if result.Supported || result.Value != nil || result.Reason != "NoCallback" {
		t.Fatalf("GetBattery: %+v", result)
	}

	radio.errs["Sensors"] = kiss.ErrUnknownCmd
	result, err = controller.Query(context.Background(), api.ModemQueryRequest{Command: "GetSensors", Permissions: ptrFloat64(1)})
	if err != nil {
		t.Fatalf("GetSensors: %v", err)
	}
	if result.Supported || result.Reason != "UnknownCmd" {
		t.Fatalf("GetSensors: %+v", result)
	}

	// An empty sensor payload is the silent-board case.
	radio.errs["Sensors"] = nil
	result, err = controller.Query(context.Background(), api.ModemQueryRequest{Command: "GetSensors", Permissions: ptrFloat64(1)})
	if err != nil {
		t.Fatalf("GetSensors: %v", err)
	}
	if result.Supported || result.Reason != "NoData" {
		t.Fatalf("GetSensors empty: %+v", result)
	}
}

func TestModemQueryTransportClasses(t *testing.T) {
	controller := modemTestController(newFakeRadio(), &fakeScheduler{}, "")
	radio := controller.radio.(*fakeModemRadio)
	radio.errs["GetCurrentRSSI"] = context.DeadlineExceeded
	_, err := controller.Query(context.Background(), api.ModemQueryRequest{Command: "GetCurrentRssi"})
	if class := modemClass(t, err); class.Class != api.ModemErrorTimeout {
		t.Fatalf("timeout class: %d", class.Class)
	}

	radio.errs["IsChannelBusy"] = fmt.Errorf("write failed")
	_, err = controller.Query(context.Background(), api.ModemQueryRequest{Command: "IsChannelBusy"})
	if class := modemClass(t, err); class.Class != api.ModemErrorRejected {
		t.Fatalf("write failure class: %d", class.Class)
	}

	// A lost link is 503, and busy-with-a-change is 409.
	controller.manager.Fail(errors.New("serial port gone"))
	_, err = controller.Query(context.Background(), api.ModemQueryRequest{Command: "Ping"})
	if class := modemClass(t, err); class.Class != api.ModemErrorLinkDown {
		t.Fatalf("link down class: %d", class.Class)
	}
	live := &fakeScheduler{}
	second := modemTestController(newFakeRadio(), live, "")
	second.applying.Store(true)
	_, err = second.Query(context.Background(), api.ModemQueryRequest{Command: "Ping"})
	if class := modemClass(t, err); class.Class != api.ModemErrorConflict {
		t.Fatalf("query during a change: %d", class.Class)
	}
}

// --- Snapshot ---------------------------------------------------------------

func TestModemSnapshotServesCacheNotTheModem(t *testing.T) {
	radio := newFakeRadio()
	controller := modemTestController(radio, &fakeScheduler{}, "")
	snapshot, err := controller.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Link != "up" || snapshot.TxPaused || snapshot.LastChange != nil || snapshot.SignalReport != nil {
		t.Fatalf("initial snapshot: %+v", snapshot)
	}
	if snapshot.Readings != nil || len(snapshot.Unsupported) != 0 || len(snapshot.Errors) != 0 {
		t.Fatalf("initial snapshot readings: %+v", snapshot)
	}
	if snapshot.Radio == (api.ModemRadioStatus{}) && snapshot.Radio.FrequencyHz == 0 {
		t.Fatalf("radio echo missing: %+v", snapshot.Radio)
	}
	if snapshot.Radio.FrequencyHz != 869618000 || snapshot.Radio.Source != "modem_echo" {
		t.Fatalf("radio echo: %+v", snapshot.Radio)
	}
	if snapshot.CSMA != (api.ModemCSMA{TxDelayMS: 500, Persistence: 63, SlotTimeMS: 100}) {
		t.Fatalf("fallback csma: %+v", snapshot.CSMA)
	}
	if snapshot.DeviceName == nil || *snapshot.DeviceName != "Emmen KISS" || snapshot.FirmwareVersion == nil || *snapshot.FirmwareVersion != 1 {
		t.Fatalf("identity: %+v", snapshot.DeviceName)
	}
	before := len(radio.calls)
	if _, err := controller.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(radio.calls) != before {
		t.Fatalf("snapshot talked to the modem: %v", radio.calls[before:])
	}

	// A lost link shows as link down, and CSMA from the config wins over the
	// host view.
	controller.manager.Fail(errors.New("port gone"))
	controller.runtime.publish(nil, models.RadioConfig{FrequencyHz: 868000000, BandwidthHz: 125000, SpreadingFactor: 8, CodingRate: 5,
		CSMA: &models.CSMAConfig{TxDelayMS: 400, Persistence: 32, SlotTimeMS: 50}})
	snapshot, err = controller.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Link != "down" {
		t.Fatalf("link: %s", snapshot.Link)
	}
	if (snapshot.CSMA != api.ModemCSMA{TxDelayMS: 400, Persistence: 32, SlotTimeMS: 50}) {
		t.Fatalf("configured csma: %+v", snapshot.CSMA)
	}
}

func TestModemRefreshFillsReadingsAndBooksErrors(t *testing.T) {
	radio := newFakeRadio()
	radio.rssi = -106
	radio.busy = false
	radio.floor = -114
	radio.tempTenths = 324
	radio.battery = 3900
	radio.errs["BatteryMillivolts"] = &kiss.ModemError{Code: 0x03}
	controller := modemTestController(radio, &fakeScheduler{}, "")
	controller.Start()
	defer controller.Stop()

	// The loop's first refresh runs immediately; wait until it recorded.
	waitFor(t, 2*time.Second, func() bool {
		snapshot, err := controller.Snapshot(context.Background())
		return err == nil && len(snapshot.Unsupported) == 1 && snapshot.Readings != nil
	})
	snapshot, err := controller.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Unsupported) != 1 || snapshot.Unsupported[0] != "GetBattery" {
		t.Fatalf("unsupported: %v", snapshot.Unsupported)
	}
	if snapshot.Readings.RSSIDBm == nil || *snapshot.Readings.RSSIDBm != -106 || snapshot.Readings.BatteryMV != nil {
		t.Fatalf("readings: %+v", snapshot.Readings)
	}
	if snapshot.Readings.MCUTempC == nil || *snapshot.Readings.MCUTempC != 32.4 || snapshot.Readings.NoiseFloorDBm == nil || *snapshot.Readings.NoiseFloorDBm != -114 {
		t.Fatalf("readings: %+v", snapshot.Readings)
	}
	if snapshot.Readings.Stats == nil || snapshot.Readings.Stats.RX != 10 {
		t.Fatalf("readings stats: %+v", snapshot.Readings.Stats)
	}
	if snapshot.SignalReport == nil {
		t.Fatalf("signal report was not resolved by the refresh: %+v", snapshot)
	}
	if snapshot.At.IsZero() {
		t.Fatalf("at was not advanced by the refresh: %v", snapshot.At)
	}

	// A transient failure keeps the last successful value, fills the errors
	// map and does not advance at.
	radio.errs["GetCurrentRSSI"] = context.DeadlineExceeded
	radio.mu.Lock()
	radio.rssi = -90
	radio.mu.Unlock()
	controller.refresh()
	snapshot, err = controller.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Readings.RSSIDBm == nil || *snapshot.Readings.RSSIDBm != -106 {
		t.Fatalf("stale rssi: %+v", snapshot.Readings)
	}
	if message := snapshot.Errors["GetCurrentRssi"]; message == "" {
		t.Fatalf("errors: %+v", snapshot.Errors)
	}

	// Success clears the error again.
	radio.errs["GetCurrentRSSI"] = nil
	controller.refresh()
	snapshot, err = controller.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Readings.RSSIDBm == nil || *snapshot.Readings.RSSIDBm != -90 {
		t.Fatalf("fresh rssi: %+v", snapshot.Readings)
	}
	if len(snapshot.Errors) != 0 {
		t.Fatalf("errors cleared: %+v", snapshot.Errors)
	}

	// A wholly failed refresh keeps the values and freezes at.
	radio.errs = map[string]error{"Ping": context.DeadlineExceeded, "GetCurrentRSSI": context.DeadlineExceeded, "IsChannelBusy": context.DeadlineExceeded, "NoiseFloor": context.DeadlineExceeded, "GetStats": context.DeadlineExceeded, "MCUTempTenthsCelsius": context.DeadlineExceeded}
	atBefore := snapshot.At
	controller.refresh()
	snapshot, err = controller.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Readings.RSSIDBm == nil || *snapshot.Readings.RSSIDBm != -90 {
		t.Fatalf("failed refresh wiped readings: %+v", snapshot.Readings)
	}
	if !snapshot.At.Equal(atBefore) {
		t.Fatalf("wholly failed refresh advanced at: %v -> %v", atBefore, snapshot.At)
	}
	if !(len(snapshot.Errors) > 0) {
		t.Fatalf("failed refresh left no errors: %+v", snapshot.Errors)
	}
}

func waitFor(t *testing.T, within time.Duration, until func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if until() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition did not become true in time")
}

// --- Preview and apply ------------------------------------------------------

func TestModemPreviewPlansStepsAndSendsNothing(t *testing.T) {
	radio := newFakeRadio()
	controller := modemTestController(radio, &fakeScheduler{}, "")
	request := validRadioRequest
	request.TxPowerDBm = ptrFloat64(14)
	request.CSMA = &api.ModemRadioCSMA{TxDelayMS: ptrFloat64(500), Persistence: ptrFloat64(63), SlotTimeMS: ptrFloat64(100), FullDuplex: ptrBool(false)}
	steps, err := controller.Preview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(radio.calls) != 0 {
		t.Fatalf("preview sent: %v", radio.calls)
	}
	wantCommands := []string{"SetRadio", "GetRadio", "SetTxPower", "GetTxPower", "SetSignalReport", "TXDELAY", "PERSISTENCE", "SLOTTIME", "FULLDUPLEX", "Ping", "save"}
	if len(steps) != len(wantCommands) {
		t.Fatalf("steps: %+v", steps)
	}
	for i, step := range steps {
		if step.Command != wantCommands[i] {
			t.Fatalf("step %d = %s, want %s", i, step.Command, wantCommands[i])
		}
		if step.Result != "" {
			t.Fatalf("preview step %s carried %q", step.Command, step.Result)
		}
	}
	if steps[0].Detail != "869618000 Hz, 62500 Hz, SF7, CR5" {
		t.Fatalf("detail: %q", steps[0].Detail)
	}
}

func TestModemApplySuccess(t *testing.T) {
	path := modemTestConfig(t, modemTestYAML)
	radio := newFakeRadio()
	scheduler := &fakeScheduler{}
	controller := modemTestController(radio, scheduler, path)

	request := validRadioRequest
	request.FrequencyHz = ptrFloat64(868000000)
	request.BandwidthHz = ptrFloat64(125000)
	request.SpreadingFactor = ptrFloat64(8)
	request.CodingRate = ptrFloat64(5)
	request.TxPowerDBm = ptrFloat64(14)
	request.CSMA = &api.ModemRadioCSMA{TxDelayMS: ptrFloat64(500), Persistence: ptrFloat64(63), SlotTimeMS: ptrFloat64(100), FullDuplex: ptrBool(false)}
	result, err := controller.Apply(context.Background(), request)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !result.Applied || !result.Saved || result.Verified == nil || *result.Verified != "modem_echo" || result.Restored || result.TxPaused || result.Error != "" {
		t.Fatalf("result: %+v", result)
	}
	wantResults := []string{"ok", "echo_matches", "ok", "echo_matches", "ok", "sent_no_reply", "sent_no_reply", "sent_no_reply", "sent_no_reply", "pong", "ok"}
	if len(result.Steps) != len(wantResults) {
		t.Fatalf("steps: %+v", result.Steps)
	}
	for i, step := range result.Steps {
		if step.Result != wantResults[i] {
			t.Fatalf("step %s result %q, want %q", step.Command, step.Result, wantResults[i])
		}
	}

	// The runtime truth moved: echo and settings show the new values.
	radio.mu.Lock()
	applied := radio.echo == (kiss.RadioConfig{Frequency: 868000000, Bandwidth: 125000, SpreadingFactor: 8, CodingRate: 5})
	power := radio.power
	radio.mu.Unlock()
	if !applied || power != 14 {
		t.Fatalf("modem state: %+v power %d", radio.echo, power)
	}
	settings := controller.runtime.settingsValue()
	if settings.FrequencyHz != 868000000 || settings.TxPowerDBm == nil || *settings.TxPowerDBm != 14 || settings.CSMA == nil || settings.CSMA.TxDelayMS != 500 {
		t.Fatalf("published settings: %+v", settings)
	}
	if scheduler.pauseState() {
		t.Fatal("scheduler stayed paused after a successful apply")
	}

	// kam.yml was rewritten with the new radio values and keeps the rest.
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := models.ParseKamConfig(stored)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Radio.FrequencyHz != 868000000 || cfg.Radio.TxPowerDBm == nil || *cfg.Radio.TxPowerDBm != 14 {
		t.Fatalf("saved radio: %+v", cfg.Radio)
	}
	if cfg.Radio.Port != "/dev/ttyUSB0" || cfg.Radio.Baud != 115200 || len(cfg.Channels) != 1 || cfg.Channels[0].Name != "#ops" {
		t.Fatalf("saved config: %+v", cfg)
	}
	if snapshot, err := controller.Snapshot(context.Background()); err != nil || snapshot.LastChange == nil || !snapshot.LastChange.Applied {
		t.Fatalf("last change: %+v err %v", snapshot.LastChange, err)
	}
}

func TestModemApplyConflictWhileBusy(t *testing.T) {
	radio := newFakeRadio()
	scheduler := &fakeScheduler{}
	controller := modemTestController(radio, scheduler, "")
	controller.applying.Store(true)
	_, err := controller.Apply(context.Background(), validRadioRequest)
	if class := modemClass(t, err); class.Class != api.ModemErrorConflict {
		t.Fatalf("busy apply: %d", class.Class)
	}
	if len(radio.calls) != 0 {
		t.Fatalf("busy apply sent: %v", radio.calls)
	}
	if scheduler.pauses != 0 {
		t.Fatal("busy apply paused the scheduler")
	}
}

func TestModemApplyLinkDown(t *testing.T) {
	radio := newFakeRadio()
	scheduler := &fakeScheduler{}
	controller := modemTestController(radio, scheduler, "")
	controller.manager.Fail(errors.New("port gone"))
	_, err := controller.Apply(context.Background(), validRadioRequest)
	if class := modemClass(t, err); class.Class != api.ModemErrorLinkDown {
		t.Fatalf("link down apply: %d", class.Class)
	}
	if len(radio.calls) != 0 || scheduler.pauses != 0 {
		t.Fatalf("link down apply touched the node: %v", radio.calls)
	}
}

func TestModemApplyValidationBeforeTheModem(t *testing.T) {
	radio := newFakeRadio()
	scheduler := &fakeScheduler{}
	controller := modemTestController(radio, scheduler, "")
	bad := validRadioRequest
	bad.CodingRate = nil
	_, err := controller.Apply(context.Background(), bad)
	if class := modemClass(t, err); class.Class != api.ModemErrorInvalid {
		t.Fatalf("invalid apply: %d", class.Class)
	}
	if len(radio.calls) != 0 || scheduler.pauses != 0 {
		t.Fatal("invalid apply touched the modem or paused TX")
	}
}

func TestModemApplyEchoMismatchRestores(t *testing.T) {
	path := modemTestConfig(t, modemTestYAML)
	radio := newFakeRadio()
	// The modem answers a mismatched echo only for the apply's echo check; the
	// capture and the restore's re-read behave normally.
	radio.onSet = func() {
		radio.mu.Lock()
		radio.mismatch = true
		radio.mu.Unlock()
	}
	scheduler := &fakeScheduler{}
	controller := modemTestController(radio, scheduler, path)
	result, err := controller.Apply(context.Background(), validRadioRequest)
	if class := modemClass(t, err); class.Class != api.ModemErrorRejected || result.Verified == nil || *result.Verified != "modem_echo" {
		t.Fatalf("mismatch result: %+v (%d %v)", result, class.Class, err)
	}
	if result.Applied || result.Saved || !result.Restored || result.TxPaused {
		t.Fatalf("mismatch result: %+v", result)
	}
	if message := modemClass(t, err).Message; !strings.Contains(message, "echo mismatch") {
		t.Fatalf("message: %q", message)
	}
	var sawMismatch, sawRestore bool
	for _, step := range result.Steps {
		if step.Command == "GetRadio" && step.Result == "echo_mismatch" {
			sawMismatch = true
		}
		if step.Command == "restore" && step.Result == "echo_matches" {
			sawRestore = true
		}
	}
	if !sawMismatch || !sawRestore {
		t.Fatalf("steps: %+v", result.Steps)
	}
	// The previous settings were written back to the modem and the file is
	// untouched.
	radio.mu.Lock()
	defer radio.mu.Unlock()
	if radio.echo != (kiss.RadioConfig{Frequency: 869618000, Bandwidth: 62500, SpreadingFactor: 7, CodingRate: 5}) {
		t.Fatalf("restored radio: %+v", radio.echo)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != modemTestYAML {
		t.Fatalf("kam.yml was rewritten: %s", stored)
	}
	if scheduler.pauseState() {
		t.Fatal("TX stayed paused after a confirmed restore")
	}
}

func TestModemApplySaveFailureRestores(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into read-only directories")
	}
	path := modemTestConfig(t, modemTestYAML)
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	radio := newFakeRadio()
	scheduler := &fakeScheduler{}
	controller := modemTestController(radio, scheduler, path)
	result, err := controller.Apply(context.Background(), validRadioRequest)
	if class := modemClass(t, err); class.Class != api.ModemErrorInternal || class.Result == nil {
		t.Fatalf("save failure: %+v (%d)", result, class.Class)
	}
	if result.Applied || result.Saved || !result.Restored || result.TxPaused || result.Verified == nil || *result.Verified != "modem_echo" {
		t.Fatalf("save failure result: %+v", result)
	}
	steps := result.Steps
	if len(steps) < 2 || steps[len(steps)-2].Command != "save" || steps[len(steps)-2].Result != "failed" || steps[len(steps)-1].Result != "echo_matches" {
		t.Fatalf("steps: %+v", steps)
	}
	// The modem got the previous settings back.
	radio.mu.Lock()
	defer radio.mu.Unlock()
	if radio.echo != (kiss.RadioConfig{Frequency: 869618000, Bandwidth: 62500, SpreadingFactor: 7, CodingRate: 5}) {
		t.Fatalf("restored radio: %+v", radio.echo)
	}
}

func TestModemApplyRestoreFailureLeavesTXPaused(t *testing.T) {
	path := modemTestConfig(t, modemTestYAML)
	radio := newFakeRadio()
	// The apply's SetSignalReport fails, and the restore's SetRadio (the
	// second SetRadio call) times out.
	radio.errs["SetSignalReport"] = context.DeadlineExceeded
	radio.failFrom["SetRadio"] = 2
	scheduler := &fakeScheduler{}
	controller := modemTestController(radio, scheduler, path)
	result, err := controller.Apply(context.Background(), validRadioRequest)
	if class := modemClass(t, err); class.Class != api.ModemErrorTimeout {
		t.Fatalf("restore failure: %d %v", class.Class, err)
	}
	if result.Applied || result.Saved || result.Restored || !result.TxPaused || result.Verified != nil {
		t.Fatalf("restore failure result: %+v", result)
	}
	last := result.Steps[len(result.Steps)-1]
	if last.Command != "restore" || last.Result != "timeout" {
		t.Fatalf("restore step: %+v", last)
	}
	if !scheduler.pauseState() || scheduler.resumes != 0 {
		t.Fatal("the scheduler must stay paused through a failed restore")
	}
	if !controller.blocked() {
		t.Fatal("the controller must report blocked during an uncertain state")
	}
	if snapshot, snapshotErr := controller.Snapshot(context.Background()); snapshotErr != nil || !snapshot.TxPaused || snapshot.LastChange == nil {
		t.Fatalf("snapshot after failure: %+v %v", snapshot.LastChange, snapshotErr)
	}
}

func TestModemApplyFileChangedIsAConflict(t *testing.T) {
	path := modemTestConfig(t, modemTestYAML)
	radio := newFakeRadio()
	scheduler := &fakeScheduler{}
	controller := modemTestController(radio, scheduler, path)
	changed := strings.Replace(modemTestYAML, "node_name: test", "node_name: edited", 1)
	radio.onSet = func() {
		if err := os.WriteFile(path, []byte(changed), 0o644); err != nil {
			t.Error(err)
		}
	}
	result, err := controller.Apply(context.Background(), validRadioRequest)
	if class := modemClass(t, err); class.Class != api.ModemErrorConflict {
		t.Fatalf("file changed: %d %v", class.Class, err)
	}
	if result.Applied || result.Saved || !result.Restored || result.TxPaused || result.Verified == nil || *result.Verified != "modem_echo" {
		t.Fatalf("file changed result: %+v", result)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != changed {
		t.Fatal("the concurrent edit was overwritten")
	}
}

func TestModemApplyAwaitIdleTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("uses real timers")
	}
	path := modemTestConfig(t, modemTestYAML)
	radio := newFakeRadio()
	scheduler := &fakeScheduler{}
	scheduler.inFlight = true
	controller := modemTestController(radio, scheduler, path)

	savedBudget := modemApplyBudget
	modemApplyBudget = 150 * time.Millisecond
	defer func() { modemApplyBudget = savedBudget }()
	result, err := controller.Apply(context.Background(), validRadioRequest)
	if class := modemClass(t, err); class.Class != api.ModemErrorTimeout {
		t.Fatalf("await idle: %d %v", class.Class, err)
	}
	if result.Applied || len(radio.calls) != 0 {
		t.Fatalf("the modem was touched: %+v / %v", result, radio.calls)
	}
	if scheduler.pauseState() {
		t.Fatal("nothing was sent; TX must resume")
	}
}

// The apply ignores the caller's context: a disconnected client must not abort
// a half-applied change. The request context is cancelled while the modem
// steps run; apply still completes and saves.
func TestModemApplyIgnoresCallerCancellation(t *testing.T) {
	path := modemTestConfig(t, modemTestYAML)
	radio := newFakeRadio()
	scheduler := &fakeScheduler{}
	controller := modemTestController(radio, scheduler, path)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := controller.Apply(ctx, validRadioRequest)
	if err != nil {
		t.Fatalf("apply with a cancelled caller: %v", err)
	}
	if !result.Applied || !result.Saved {
		t.Fatalf("result: %+v", result)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := models.ParseKamConfig(stored)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Radio.FrequencyHz != 869618000 {
		t.Fatalf("saved radio: %+v", cfg.Radio)
	}
}

// A later successful apply clears a pause left by an earlier failed change
// (§7.5.1 step 7).
func TestModemApplySuccessClearsTXPause(t *testing.T) {
	path := modemTestConfig(t, modemTestYAML)
	radio := newFakeRadio()
	scheduler := &fakeScheduler{}
	controller := modemTestController(radio, scheduler, path)

	// Leave the uncertain state from an earlier failed change.
	controller.setTxPaused(true)
	if !controller.blocked() {
		t.Fatal("the controller must be blocked while paused")
	}
	result, err := controller.Apply(context.Background(), validRadioRequest)
	if err != nil {
		t.Fatalf("apply after a pause: %v", err)
	}
	if !result.Applied || result.TxPaused {
		t.Fatalf("result: %+v", result)
	}
	if controller.blocked() {
		t.Fatal("a successful apply must clear the pause")
	}
	if scheduler.pauseState() {
		t.Fatal("the scheduler must dispatch again after a successful apply")
	}
	snapshot, err := controller.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.TxPaused {
		t.Fatalf("snapshot tx_paused: %+v", snapshot)
	}
}

func TestModemBlockedSkipsProbeWork(t *testing.T) {
	scheduler := &fakeScheduler{}
	controller := modemTestController(newFakeRadio(), scheduler, "")
	controller.applying.Store(true)
	if !controller.blocked() {
		t.Fatal("an apply must block the probe")
	}
	controller.applying.Store(false)
	if controller.blocked() {
		t.Fatal("idle controller must not block the probe")
	}
	controller.setTxPaused(true)
	if !controller.blocked() {
		t.Fatal("a paused controller must block the probe")
	}
}
