// The modem capability behind the /v1/modem routes (FeatureKamApi.md §7.5,
// requirements API-MD-1..11): a cached snapshot refreshed every 10 s, one
// read-only query command at a time, and guarded radio changes with save and
// recovery. The controller owns the state and the modem conversation; the
// transport in internal/api only shapes and decodes. See docs/APIdetails.md.
package node

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"kam.bytecoder.nl/internal/api"
	"kam.bytecoder.nl/internal/kiss"
	"kam.bytecoder.nl/internal/models"
)

// Budgets for the modem work; vars so tests can shorten them.
var (
	// modemRefreshInterval is how often the controller refreshes the cached
	// readings; a snapshot request itself never talks to the modem.
	modemRefreshInterval = 10 * time.Second
	// modemRefreshBudget bounds one refresh batch of commands.
	modemRefreshBudget = 8 * time.Second
	// §7.5.1: at most 10 s for the change and a separate 5 s for recovery,
	// both independent of the HTTP request.
	modemApplyBudget   = 10 * time.Second
	modemRestoreBudget = 5 * time.Second
	// modemErrorSummaryBytes bounds an error string in the snapshot, as the
	// event ring bounds error summaries.
	modemErrorSummaryBytes = 256
	// modemQueryPingDetail is the value the Ping command answers with.
	modemQueryPingDetail = "pong"
)

// modemRadio is the part of kiss.RadioClient the controller needs. The
// concrete client is handed in so tests can fake the modem.
type modemRadio interface {
	Ping(ctx context.Context) error
	GetRadio(ctx context.Context) (kiss.RadioConfig, error)
	SetRadio(ctx context.Context, config kiss.RadioConfig) error
	GetTxPower(ctx context.Context) (uint8, error)
	SetTxPower(ctx context.Context, dbm uint8) error
	GetSignalReport(ctx context.Context) (bool, error)
	SetSignalReport(ctx context.Context, enabled bool) (bool, error)
	SetTxDelay(ctx context.Context, milliseconds int) error
	SetPersistence(ctx context.Context, value uint8) error
	SetSlotTime(ctx context.Context, milliseconds int) error
	SetFullDuplex(ctx context.Context, enabled bool) error
	GetCurrentRSSI(ctx context.Context) (int8, error)
	IsChannelBusy(ctx context.Context) (bool, error)
	AirtimeMillis(ctx context.Context, length uint8) (uint32, error)
	NoiseFloor(ctx context.Context) (int16, bool, error)
	GetVersion(ctx context.Context) (kiss.FirmwareVersion, error)
	GetStats(ctx context.Context) (kiss.ModemStats, error)
	BatteryMillivolts(ctx context.Context) (uint16, error)
	MCUTempTenthsCelsius(ctx context.Context) (int16, error)
	Sensors(ctx context.Context, permissions uint8) ([]byte, error)
	DeviceName(ctx context.Context) (string, error)
	CachedIdentity() (*string, *uint8)
}

var _ modemRadio = (*kiss.RadioClient)(nil)

// modemScheduler is the part of TxScheduler the controller needs: suspend and
// resume dispatches, and wait for the in-flight frame.
type modemScheduler interface {
	Pause()
	Resume()
	AwaitIdle(ctx context.Context) error
}

var _ modemScheduler = (*kiss.TxScheduler)(nil)

// previousSettings is the state captured before a change touches the modem;
// a failed change restores it.
type previousSettings struct {
	echo   *kiss.RadioConfig
	power  *uint8
	signal *bool
	csma   kiss.CSMAConfig
}

// modemController implements api.ModemSource for one KISS modem.
type modemController struct {
	radio      modemRadio
	scheduler  modemScheduler
	manager    *kiss.Manager
	runtime    *radioRuntime
	configPath string

	applying   atomic.Bool
	refreshing atomic.Bool
	stop       chan struct{}
	stopOnce   sync.Once
	refreshNow chan struct{}
	started    time.Time

	mu            sync.Mutex
	txPaused      bool
	lastChange    *api.ModemRadioResult
	at            time.Time
	readings      api.ModemReadings
	readingsReady bool
	signalReport  *bool
	txPowerEcho   *uint8
	unsupported   map[string]struct{}
	errors        map[string]string
}

var _ api.ModemSource = (*modemController)(nil)

func newModemController(radio modemRadio, scheduler modemScheduler, manager *kiss.Manager, runtime *radioRuntime, configPath string, initialSignalReport *bool) *modemController {
	controller := &modemController{
		radio:       radio,
		scheduler:   scheduler,
		manager:     manager,
		runtime:     runtime,
		configPath:  configPath,
		stop:        make(chan struct{}),
		refreshNow:  make(chan struct{}, 1),
		started:     time.Now().UTC().Truncate(time.Millisecond),
		unsupported: map[string]struct{}{},
		errors:      map[string]string{},
	}
	controller.signalReport = initialSignalReport
	return controller
}

// Start launches the refresh loop; the first refresh runs immediately.
func (c *modemController) Start() {
	go c.loop()
}

// Stop ends the refresh loop. In-flight modem commands end on their own
// command timeouts; the caller never waits on this.
func (c *modemController) Stop() {
	c.stopOnce.Do(func() { close(c.stop) })
}

func (c *modemController) loop() {
	ticker := time.NewTicker(modemRefreshInterval)
	defer ticker.Stop()
	c.refresh()
	for {
		select {
		case <-c.stop:
			return
		case <-c.refreshNow:
			c.refresh()
		case <-ticker.C:
			c.refresh()
		}
	}
}

// blocked reports whether a radio change runs or TX sits paused; the reset
// probe skips its work then, because it must not touch the modem, and must not
// silently clear an uncertain state (§7.5.1 step 7).
func (c *modemController) blocked() bool {
	if c.applying.Load() {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.txPaused
}

// --- Snapshot ---------------------------------------------------------------

// Snapshot serves the cached state; it never sends a modem command (API-MD-1).
func (c *modemController) Snapshot(ctx context.Context) (api.ModemSnapshot, error) {
	echo := c.runtime.echoValue()
	settings := c.runtime.settingsValue()

	snapshot := api.ModemSnapshot{Link: "up"}
	if c.manager.Err() != nil {
		snapshot.Link = "down"
	}
	if echo != nil {
		snapshot.Radio = api.ModemRadioStatus{
			FrequencyHz: echo.Frequency, BandwidthHz: echo.Bandwidth,
			SpreadingFactor: echo.SpreadingFactor, CodingRate: echo.CodingRate,
			Source: "modem_echo",
		}
	} else {
		snapshot.Radio = api.ModemRadioStatus{Source: "modem_echo"}
	}
	snapshot.Radio.TxPowerDBM = settings.TxPowerDBm
	snapshot.DeviceName, snapshot.FirmwareVersion = c.radio.CachedIdentity()
	snapshot.CSMA = modemCSMAWire(settings.CSMA, c.manager.CurrentCSMA())

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at.IsZero() {
		snapshot.At = c.started
	} else {
		snapshot.At = c.at
	}
	snapshot.SignalReport = c.signalReport
	if c.readingsReady {
		readings := c.readings
		snapshot.Readings = &readings
	}
	snapshot.TxPaused = c.txPaused
	snapshot.LastChange = c.lastChange
	snapshot.Unsupported = make([]string, 0, len(c.unsupported))
	for command := range c.unsupported {
		snapshot.Unsupported = append(snapshot.Unsupported, command)
	}
	sort.Strings(snapshot.Unsupported)
	snapshot.Errors = make(map[string]string, len(c.errors))
	for command, message := range c.errors {
		snapshot.Errors[command] = message
	}
	if snapshot.Radio.TxPowerDBM == nil {
		snapshot.Radio.TxPowerDBM = c.txPowerEcho
	}
	return snapshot, nil
}

// modemCSMAWire maps the configured CSMA values to the wire shape; when the
// config leaves CSMA out entirely, the host's view of the modem serves.
func modemCSMAWire(configured *models.CSMAConfig, runtime kiss.CSMAConfig) api.ModemCSMA {
	if configured != nil {
		return api.ModemCSMA{
			TxDelayMS: configured.TxDelayMS, Persistence: configured.Persistence,
			SlotTimeMS: configured.SlotTimeMS, FullDuplex: configured.FullDuplex,
		}
	}
	return api.ModemCSMA{
		TxDelayMS: runtime.TxDelayMS, Persistence: int(runtime.Persistence),
		SlotTimeMS: runtime.SlotTimeMS, FullDuplex: runtime.FullDuplex,
	}
}

// --- Query ------------------------------------------------------------------

// modemQueryCommands is the allowlist for POST /v1/modem/query (API-MD-2);
// queryCommandArgs names the ones that take an argument, every other command
// accepts only the command name.
var modemQueryCommands = []string{
	"Ping", "GetRadio", "GetTxPower", "GetCurrentRssi", "IsChannelBusy", "GetAirtime",
	"GetNoiseFloor", "GetVersion", "GetStats", "GetBattery", "GetMCUTemp", "GetSensors",
	"GetDeviceName", "GetSignalReport",
}

var modemQueryAllowed = func() map[string]bool {
	allowed := make(map[string]bool, len(modemQueryCommands))
	for _, command := range modemQueryCommands {
		allowed[command] = true
	}
	return allowed
}()

var queryCommandArgs = map[string]string{"GetAirtime": "length", "GetSensors": "permissions"}

// Query runs one read-only modem command now. An unsupported command is a
// normal response (supported false), never an error (API-MD-10).
func (c *modemController) Query(ctx context.Context, request api.ModemQueryRequest) (api.ModemQueryResult, error) {
	command := request.Command
	if command == "" {
		return api.ModemQueryResult{}, &api.ModemError{Class: api.ModemErrorBadRequest, Message: "command is required"}
	}
	if !modemQueryAllowed[command] {
		return api.ModemQueryResult{}, &api.ModemError{Class: api.ModemErrorBadRequest, Message: fmt.Sprintf("unknown command %q", command)}
	}
	if command != "GetAirtime" && request.Length != nil {
		return api.ModemQueryResult{}, &api.ModemError{Class: api.ModemErrorInvalid, Message: fmt.Sprintf("%s does not take length", command)}
	}
	if command != "GetSensors" && request.Permissions != nil {
		return api.ModemQueryResult{}, &api.ModemError{Class: api.ModemErrorInvalid, Message: fmt.Sprintf("%s does not take permissions", command)}
	}

	if c.applying.Load() {
		return api.ModemQueryResult{}, &api.ModemError{Class: api.ModemErrorConflict, Message: "a radio change is in progress"}
	}
	if c.manager.Err() != nil {
		return api.ModemQueryResult{}, &api.ModemError{Class: api.ModemErrorLinkDown, Message: "modem link is down", Cause: c.manager.Err()}
	}

	started := time.Now()
	value, err := c.runQuery(ctx, command, request)
	if errors.Is(err, errNoSensorData) {
		return unsupportedQueryResult(command, started, "NoData"), nil
	}
	if reason, unsupported := modemUnsupportedReason(err); unsupported {
		return unsupportedQueryResult(command, started, reason), nil
	}
	if err != nil {
		var modemErr *api.ModemError
		if errors.As(err, &modemErr) {
			return api.ModemQueryResult{}, modemErr
		}
		return api.ModemQueryResult{}, c.modemClassErr(command, err)
	}
	return api.ModemQueryResult{
		Command: command, At: time.Now().UTC().Truncate(time.Millisecond),
		DurationMS: time.Since(started).Milliseconds(),
		Supported:  true, Value: value,
	}, nil
}

func unsupportedQueryResult(command string, started time.Time, reason string) api.ModemQueryResult {
	return api.ModemQueryResult{
		Command: command, At: time.Now().UTC().Truncate(time.Millisecond),
		DurationMS: time.Since(started).Milliseconds(),
		Supported:  false, Value: nil, Reason: reason,
	}
}

// runQuery dispatches one allowlisted command and returns its typed value.
func (c *modemController) runQuery(ctx context.Context, command string, request api.ModemQueryRequest) (any, error) {
	switch command {
	case "Ping":
		if err := c.radio.Ping(ctx); err != nil {
			return nil, err
		}
		return modemQueryPingDetail, nil
	case "GetRadio":
		config, err := c.radio.GetRadio(ctx)
		if err != nil {
			return nil, err
		}
		return api.ModemRadioValue{FrequencyHz: config.Frequency, BandwidthHz: config.Bandwidth, SpreadingFactor: config.SpreadingFactor, CodingRate: config.CodingRate}, nil
	case "GetTxPower":
		dbm, err := c.radio.GetTxPower(ctx)
		if err != nil {
			return nil, err
		}
		return int(dbm), nil
	case "GetCurrentRssi":
		rssi, err := c.radio.GetCurrentRSSI(ctx)
		if err != nil {
			return nil, err
		}
		return int(rssi), nil
	case "IsChannelBusy":
		busy, err := c.radio.IsChannelBusy(ctx)
		if err != nil {
			return nil, err
		}
		return busy, nil
	case "GetAirtime":
		length, err := modemQueryUint(request.Length, "length", 1, 255, command)
		if err != nil {
			return nil, err
		}
		duration, err := c.radio.AirtimeMillis(ctx, length)
		if err != nil {
			return nil, err
		}
		return int64(duration), nil
	case "GetNoiseFloor":
		floor, _, err := c.radio.NoiseFloor(ctx)
		if err != nil {
			return nil, err
		}
		return int(floor), nil
	case "GetVersion":
		version, err := c.radio.GetVersion(ctx)
		if err != nil {
			return nil, err
		}
		return api.ModemVersionValue{Version: version.Version, Reserved: version.Reserved}, nil
	case "GetStats":
		stats, err := c.radio.GetStats(ctx)
		if err != nil {
			return nil, err
		}
		return api.ModemStats{RX: stats.RX, TX: stats.TX, Errors: stats.Errors}, nil
	case "GetBattery":
		millivolts, err := c.radio.BatteryMillivolts(ctx)
		if err != nil {
			return nil, err
		}
		return int(millivolts), nil
	case "GetMCUTemp":
		tenths, err := c.radio.MCUTempTenthsCelsius(ctx)
		if err != nil {
			return nil, err
		}
		return float64(tenths) / 10, nil
	case "GetSensors":
		permissions, err := modemQueryUint(request.Permissions, "permissions", 1, 7, command)
		if err != nil {
			return nil, err
		}
		data, err := c.radio.Sensors(ctx, permissions)
		if err != nil {
			return nil, err
		}
		if len(data) == 0 {
			return nil, errNoSensorData
		}
		return api.ModemSensorsValue{Permissions: int(permissions), DataHex: hex.EncodeToString(data)}, nil
	case "GetDeviceName":
		name, err := c.radio.DeviceName(ctx)
		if err != nil {
			return nil, err
		}
		return name, nil
	case "GetSignalReport":
		enabled, err := c.radio.GetSignalReport(ctx)
		if err != nil {
			return nil, err
		}
		return enabled, nil
	default:
		return nil, &api.ModemError{Class: api.ModemErrorBadRequest, Message: fmt.Sprintf("unknown command %q", command)}
	}
}

// errNoSensorData marks a GetSensors response with no payload: the board has
// no sensors (API-MD-10).
var errNoSensorData = errors.New("the modem returned no sensor data")

// modemUnsupportedReason reports whether a command failure means the board
// does not support the command (a normal response, never an error), and why.
func modemUnsupportedReason(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	var modemError *kiss.ModemError
	if errors.As(err, &modemError) {
		switch modemError.Code {
		case 0x03: // MODEM_ERROR_NO_CALLBACK
			return "NoCallback", true
		case 0x05: // MODEM_ERROR_UNKNOWN_CMD
			return "UnknownCmd", true
		}
		return "", false
	}
	switch {
	case errors.Is(err, kiss.ErrNoCallback):
		return "NoCallback", true
	case errors.Is(err, kiss.ErrUnknownCmd), errors.Is(err, kiss.ErrUnsupported):
		return "UnknownCmd", true
	}
	return "", false
}

// modemClassErr maps a command failure onto the transport vocabulary: a timeout
// is 504, the link being lost is 503, everything else is 502.
func (c *modemController) modemClassErr(command string, err error) *api.ModemError {
	class := api.ModemErrorRejected
	if linkErr := c.manager.Err(); linkErr != nil {
		return &api.ModemError{Class: api.ModemErrorLinkDown, Message: fmt.Sprintf("%s: modem link is down", command), Cause: err}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, kiss.ErrModemUnresponsive) {
		class = api.ModemErrorTimeout
	}
	return &api.ModemError{Class: class, Message: fmt.Sprintf("%s: %v", command, err), Cause: err}
}

// modemQueryUint validates one integer command argument: a missing, fractional
// or out-of-range value is a semantic (422) refusal.
func modemQueryUint(value *float64, name string, min, max float64, command string) (uint8, error) {
	if value == nil {
		return 0, &api.ModemError{Class: api.ModemErrorInvalid, Message: fmt.Sprintf("%s is required for %s", name, command)}
	}
	if *value != math.Trunc(*value) || *value < min || *value > max {
		return 0, &api.ModemError{Class: api.ModemErrorInvalid, Message: fmt.Sprintf("%s must be an integer between %d and %d, got %v", name, int(min), int(max), *value)}
	}
	return uint8(*value), nil
}

// --- Preview and apply ------------------------------------------------------

// Preview validates the request and returns the planned steps; nothing is
// sent, paused or saved.
func (c *modemController) Preview(ctx context.Context, request api.ModemRadioRequest) ([]api.ModemStep, error) {
	wanted, txPower, csma, err := decodeRadioRequest(request)
	if err != nil {
		return nil, err
	}
	return planSteps(wanted, txPower, csma), nil
}

// planSteps builds the ordered command list both the preview and the real
// apply follow; result strings are empty for a preview.
func planSteps(wanted kiss.RadioConfig, txPower *uint8, csma *kiss.CSMAConfig) []api.ModemStep {
	steps := []api.ModemStep{
		{Command: "SetRadio", Detail: radioDetail(wanted)},
		{Command: "GetRadio"},
	}
	if txPower != nil {
		steps = append(steps,
			api.ModemStep{Command: "SetTxPower", Detail: fmt.Sprintf("%d dBm", *txPower)},
			api.ModemStep{Command: "GetTxPower"},
		)
	}
	steps = append(steps, api.ModemStep{Command: "SetSignalReport"})
	if csma != nil {
		steps = append(steps,
			api.ModemStep{Command: "TXDELAY", Detail: fmt.Sprintf("%d ms", csma.TxDelayMS)},
			api.ModemStep{Command: "PERSISTENCE", Detail: fmt.Sprintf("%d", csma.Persistence)},
			api.ModemStep{Command: "SLOTTIME", Detail: fmt.Sprintf("%d ms", csma.SlotTimeMS)},
			api.ModemStep{Command: "FULLDUPLEX", Detail: duplexDetail(csma.FullDuplex)},
		)
	}
	steps = append(steps, api.ModemStep{Command: "Ping"}, api.ModemStep{Command: "save", Detail: "rewrite the radio: section in kam.yml"})
	return steps
}

func radioDetail(config kiss.RadioConfig) string {
	return fmt.Sprintf("%d Hz, %d Hz, SF%d, CR%d", config.Frequency, config.Bandwidth, config.SpreadingFactor, config.CodingRate)
}

func duplexDetail(enabled bool) string {
	if enabled {
		return "on"
	}
	return "off"
}

// decodeRadioRequest validates the entire request before anything touches the
// modem. A missing required value, a fractional number or an out-of-range
// value is a 422 refusal naming the field (API-MD-4); omitting tx_power_dbm or
// csma leaves that setting alone.
func decodeRadioRequest(request api.ModemRadioRequest) (wanted kiss.RadioConfig, txPower *uint8, csma *kiss.CSMAConfig, err error) {
	frequency, err := radioUint32(request.FrequencyHz, "frequency_hz")
	if err != nil {
		return wanted, nil, nil, err
	}
	bandwidth, err := radioUint32(request.BandwidthHz, "bandwidth_hz")
	if err != nil {
		return wanted, nil, nil, err
	}
	spreadingFactor, err := radioUint8(request.SpreadingFactor, "spreading_factor")
	if err != nil {
		return wanted, nil, nil, err
	}
	wanted = kiss.RadioConfig{Frequency: frequency, Bandwidth: bandwidth, SpreadingFactor: spreadingFactor}

	codingRate, err := radioUint8(request.CodingRate, "coding_rate")
	if err != nil {
		return wanted, nil, nil, err
	}
	wanted.CodingRate = codingRate
	if err := models.ValidateRadioParameters(wanted.Frequency, wanted.Bandwidth, wanted.SpreadingFactor, wanted.CodingRate); err != nil {
		return wanted, nil, nil, &api.ModemError{Class: api.ModemErrorInvalid, Message: err.Error(), Cause: err}
	}
	if request.TxPowerDBm != nil {
		power, err := radioUint8(request.TxPowerDBm, "tx_power_dbm")
		if err != nil {
			return wanted, nil, nil, err
		}
		if err := models.ValidateTxPowerDBm(power); err != nil {
			return wanted, nil, nil, &api.ModemError{Class: api.ModemErrorInvalid, Message: err.Error(), Cause: err}
		}
		txPower = &power
	}
	if request.CSMA != nil {
		block := request.CSMA
		if block.TxDelayMS == nil || block.Persistence == nil || block.SlotTimeMS == nil || block.FullDuplex == nil {
			return wanted, nil, nil, &api.ModemError{Class: api.ModemErrorInvalid, Message: "csma requires txdelay_ms, persistence, slot_time_ms and full_duplex"}
		}
		txDelay, err := radioInt(block.TxDelayMS, "csma.txdelay_ms")
		if err != nil {
			return wanted, nil, nil, err
		}
		persistence, err := radioInt(block.Persistence, "csma.persistence")
		if err != nil {
			return wanted, nil, nil, err
		}
		slotTime, err := radioInt(block.SlotTimeMS, "csma.slot_time_ms")
		if err != nil {
			return wanted, nil, nil, err
		}
		validated := models.CSMAConfig{TxDelayMS: txDelay, Persistence: persistence, SlotTimeMS: slotTime, FullDuplex: *block.FullDuplex}
		if err := validated.Validate(); err != nil {
			return wanted, nil, nil, &api.ModemError{Class: api.ModemErrorInvalid, Message: err.Error(), Cause: err}
		}
		csma = &kiss.CSMAConfig{
			TxDelayMS: validated.TxDelayMS, Persistence: uint8(validated.Persistence),
			SlotTimeMS: validated.SlotTimeMS, FullDuplex: validated.FullDuplex,
		}
	}
	return wanted, txPower, csma, nil
}

// radioUint32 validates one required integer field in the uint32 range; the
// LoRa range itself is the model's validation.
func radioUint32(fields *float64, name string) (uint32, error) {
	if fields == nil {
		return 0, &api.ModemError{Class: api.ModemErrorInvalid, Message: fmt.Sprintf("%s is required", name)}
	}
	value := *fields
	if value != math.Trunc(value) || value < 0 || value > math.MaxUint32 {
		return 0, &api.ModemError{Class: api.ModemErrorInvalid, Message: fmt.Sprintf("%s must be an integer between 0 and 4294967295, got %v", name, value)}
	}
	return uint32(value), nil
}

// radioUint8 validates one required integer field in the uint8 range.
func radioUint8(fields *float64, name string) (uint8, error) {
	if fields == nil {
		return 0, &api.ModemError{Class: api.ModemErrorInvalid, Message: fmt.Sprintf("%s is required", name)}
	}
	value := *fields
	if value != math.Trunc(value) || value < 0 || value > math.MaxUint8 {
		return 0, &api.ModemError{Class: api.ModemErrorInvalid, Message: fmt.Sprintf("%s must be an integer between 0 and 255, got %v", name, value)}
	}
	return uint8(value), nil
}

// radioInt validates one required integer field of the CSMA block.
func radioInt(fields *float64, name string) (int, error) {
	if fields == nil {
		return 0, &api.ModemError{Class: api.ModemErrorInvalid, Message: fmt.Sprintf("%s is required", name)}
	}
	value := *fields
	if value != math.Trunc(value) || value < math.MinInt32 || value > math.MaxInt32 {
		return 0, &api.ModemError{Class: api.ModemErrorInvalid, Message: fmt.Sprintf("%s must be an integer, got %v", name, value)}
	}
	return int(value), nil
}

// --- The refresh loop -------------------------------------------------------

// refresh collects one batch of readings. It never blocks a radio change:
// the batch stops as soon as one starts, and a wholly failed batch keeps the
// last successful values and does not advance at (API-MD-1, §7.5).
func (c *modemController) refresh() {
	if !c.refreshing.CompareAndSwap(false, true) {
		return
	}
	defer c.refreshing.Store(false)
	if c.applying.Load() {
		return
	}
	if c.manager.Err() != nil {
		return
	}
	batch, cancel := context.WithTimeout(context.Background(), modemRefreshBudget)
	defer cancel()

	succeeded := false
	// run performs one exchange and, on success, applies the result to the
	// state under the lock (the apply func runs inside record's critical
	// section, so it must not lock itself).
	run := func(command string, call func(context.Context) error, apply func()) {
		if batch.Err() != nil || c.applying.Load() {
			return
		}
		c.record(command, call(batch), func() { apply(); succeeded = true })
	}

	c.mu.Lock()
	needSignal := c.signalReport == nil
	needPower := c.txPowerEcho == nil && c.runtime.settingsValue().TxPowerDBm == nil
	c.mu.Unlock()

	run("Ping", func(ctx context.Context) error { return c.radio.Ping(ctx) }, func() {})

	var rssi int8
	run("GetCurrentRssi", func(ctx context.Context) error { var err error; rssi, err = c.radio.GetCurrentRSSI(ctx); return err },
		func() { c.readings.RSSIDBm = &rssi })

	var busy bool
	run("IsChannelBusy", func(ctx context.Context) error { var err error; busy, err = c.radio.IsChannelBusy(ctx); return err },
		func() { c.readings.ChannelBusy = &busy })

	var floor int16
	var calibrated bool
	run("GetNoiseFloor", func(ctx context.Context) error {
		var err error
		floor, calibrated, err = c.radio.NoiseFloor(ctx)
		return err
	},
		func() {
			if calibrated {
				c.readings.NoiseFloorDBm = &floor
				c.readings.NoiseFloorCalibrated = true
			} else {
				c.readings.NoiseFloorDBm = nil
				c.readings.NoiseFloorCalibrated = false
			}
		})

	var stats kiss.ModemStats
	run("GetStats", func(ctx context.Context) error { var err error; stats, err = c.radio.GetStats(ctx); return err },
		func() { c.readings.Stats = &api.ModemStats{RX: stats.RX, TX: stats.TX, Errors: stats.Errors} })

	var millivolts uint16
	run("GetBattery", func(ctx context.Context) error {
		var err error
		millivolts, err = c.radio.BatteryMillivolts(ctx)
		return err
	},
		func() { c.readings.BatteryMV = &millivolts })

	var tenths int16
	run("GetMCUTemp", func(ctx context.Context) error {
		var err error
		tenths, err = c.radio.MCUTempTenthsCelsius(ctx)
		return err
	},
		func() { celsius := float64(tenths) / 10; c.readings.MCUTempC = &celsius })

	if needSignal {
		var enabled bool
		run("GetSignalReport", func(ctx context.Context) error {
			var err error
			enabled, err = c.radio.GetSignalReport(ctx)
			return err
		},
			func() { c.signalReport = &enabled })
	}
	if needPower {
		var dbm uint8
		run("GetTxPower", func(ctx context.Context) error { var err error; dbm, err = c.radio.GetTxPower(ctx); return err },
			func() { c.txPowerEcho = &dbm })
	}

	if succeeded {
		c.mu.Lock()
		c.at = time.Now().UTC().Truncate(time.Millisecond)
		c.readingsReady = true
		c.mu.Unlock()
	}
}

// record bookkeeps one refresh command under the state lock: success clears
// the entry, an unsupported board clears the error and marks the command, and
// a transient failure keeps the last successful value (errors API §7.5).
func (c *modemController) record(command string, err error, apply func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, unsupported := modemUnsupportedReason(err)
	switch {
	case err == nil:
		delete(c.errors, command)
		delete(c.unsupported, command)
		apply()
	case unsupported:
		delete(c.errors, command)
		c.unsupported[command] = struct{}{}
	default:
		c.errors[command] = modemErrorSummary(err)
	}
}

func (c *modemController) setSignalReport(enabled bool) {
	value := enabled
	c.mu.Lock()
	c.signalReport = &value
	c.mu.Unlock()
}

func (c *modemController) setTxPaused(paused bool) {
	c.mu.Lock()
	c.txPaused = paused
	c.mu.Unlock()
}

func (c *modemController) setLastChange(result *api.ModemRadioResult) {
	c.mu.Lock()
	c.lastChange = result
	c.mu.Unlock()
}

func (c *modemController) signalRefresh() {
	select {
	case c.refreshNow <- struct{}{}:
	default:
	}
}

// modemErrorSummary bounds an error string, cutting at a UTF-8 boundary.
func modemErrorSummary(err error) string {
	text := err.Error()
	if len(text) <= modemErrorSummaryBytes {
		return text
	}
	cut := text[:modemErrorSummaryBytes]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// --- Apply and recovery -----------------------------------------------------

// Apply validates, then follows §7.5.1: gate, pause TX, wait for the in-flight
// frame, set and echo, save, publish and resume — with restoration on any
// failure after a modem command. The operation runs on its own budgets and
// ignores ctx: a disconnected client must not abort a half-applied change.
func (c *modemController) Apply(ctx context.Context, request api.ModemRadioRequest) (api.ModemRadioResult, error) {
	wanted, txPower, csma, err := decodeRadioRequest(request)
	if err != nil {
		return api.ModemRadioResult{}, err
	}
	if !c.applying.CompareAndSwap(false, true) {
		return api.ModemRadioResult{}, &api.ModemError{Class: api.ModemErrorConflict, Message: "a radio change is already in progress"}
	}
	defer c.applying.Store(false)
	if err := c.manager.Err(); err != nil {
		return api.ModemRadioResult{}, &api.ModemError{Class: api.ModemErrorLinkDown, Message: "modem link is down", Cause: err}
	}
	fileBefore, err := os.ReadFile(c.configPath)
	if err != nil {
		return api.ModemRadioResult{}, &api.ModemError{Class: api.ModemErrorInternal, Message: "no usable kam.yml to save into", Cause: err}
	}

	op, cancelOp := context.WithTimeout(context.Background(), modemApplyBudget)
	defer cancelOp()
	var steps []api.ModemStep
	add := func(command, detail, result string) {
		steps = append(steps, api.ModemStep{Command: command, Detail: detail, Result: result})
	}

	// Suspend TX dispatch and probe/refresh work for the whole change.
	c.scheduler.Pause()
	defer func() {
		// The success and recovery paths resume explicitly; a paused
		// controller must be resumed only when the restored state is
		// confirmed, so a leftover pause survives this defer.
		if !c.pausedState() { // cheap best effort: resume when not marked paused
			c.scheduler.Resume()
		}
	}()

	if err := c.scheduler.AwaitIdle(op); err != nil {
		add("AwaitIdle", "", "timeout")
		result := api.ModemRadioResult{Error: "the radio did not go idle in time", Verified: nil, Steps: steps}
		return failResult(c, result, api.ModemErrorTimeout, result.Error)
	}

	prev, err := c.capture(op)
	if err != nil {
		add("GetRadio", "", stepResultWord(err))
		result := api.ModemRadioResult{Error: fmt.Sprintf("GetRadio: %v", err), Verified: nil, Steps: steps}
		return failResult(c, result, c.modemClassErr("GetRadio", err).Class, result.Error)
	}

	// From here the modem is touched: every failure restores.
	radioSent, powerSent, csmaSent := false, false, false
	setRadio := func() error { return c.radio.SetRadio(op, wanted) }
	if err := setRadio(); err != nil {
		radioSent = true
		add("SetRadio", radioDetail(wanted), stepResultWord(err))
		return c.recover(prev, radioSent, powerSent, csmaSent, steps, c.modemClassErr("SetRadio", err))
	}
	radioSent = true
	add("SetRadio", radioDetail(wanted), "ok")

	echo, err := c.radio.GetRadio(op)
	if err != nil {
		add("GetRadio", "", stepResultWord(err))
		return c.recover(prev, radioSent, powerSent, csmaSent, steps, c.modemClassErr("GetRadio", err))
	}
	if echo != wanted {
		add("GetRadio", "", "echo_mismatch")
		failure := &api.ModemError{Class: api.ModemErrorRejected, Message: fmt.Sprintf("radio echo mismatch: got %s, want %s", radioDetail(echo), radioDetail(wanted))}
		return c.recover(prev, radioSent, powerSent, csmaSent, steps, failure)
	}
	add("GetRadio", "", "echo_matches")

	if txPower != nil {
		powerSent = true
		if err := c.radio.SetTxPower(op, *txPower); err != nil {
			add("SetTxPower", fmt.Sprintf("%d dBm", *txPower), stepResultWord(err))
			return c.recover(prev, radioSent, powerSent, csmaSent, steps, c.modemClassErr("SetTxPower", err))
		}
		add("SetTxPower", fmt.Sprintf("%d dBm", *txPower), "ok")
		powerEcho, err := c.radio.GetTxPower(op)
		if err != nil {
			add("GetTxPower", "", stepResultWord(err))
			return c.recover(prev, radioSent, powerSent, csmaSent, steps, c.modemClassErr("GetTxPower", err))
		}
		if powerEcho != *txPower {
			add("GetTxPower", "", "echo_mismatch")
			failure := &api.ModemError{Class: api.ModemErrorRejected, Message: fmt.Sprintf("TX power echo mismatch: got %d dBm, want %d dBm", powerEcho, *txPower)}
			return c.recover(prev, radioSent, powerSent, csmaSent, steps, failure)
		}
		add("GetTxPower", "", "echo_matches")
	}

	signalReport, err := c.radio.SetSignalReport(op, true)
	if err != nil {
		add("SetSignalReport", "", stepResultWord(err))
		return c.recover(prev, radioSent, powerSent, csmaSent, steps, c.modemClassErr("SetSignalReport", err))
	}
	if !signalReport {
		add("SetSignalReport", "", "failed")
		return c.recover(prev, radioSent, powerSent, csmaSent, steps, &api.ModemError{Class: api.ModemErrorRejected, Message: "the modem did not enable signal reporting"})
	}
	add("SetSignalReport", "", "ok")
	c.setSignalReport(true)

	if csma != nil {
		csmaSent = true
		if csma.FullDuplex {
			slog.Warn("radio full_duplex bypasses CSMA; use only with suitable hardware")
		}
		settings := []struct {
			command string
			detail  string
			send    func() error
		}{
			{"TXDELAY", fmt.Sprintf("%d ms", csma.TxDelayMS), func() error { return c.radio.SetTxDelay(op, csma.TxDelayMS) }},
			{"PERSISTENCE", fmt.Sprintf("%d", csma.Persistence), func() error { return c.radio.SetPersistence(op, csma.Persistence) }},
			{"SLOTTIME", fmt.Sprintf("%d ms", csma.SlotTimeMS), func() error { return c.radio.SetSlotTime(op, csma.SlotTimeMS) }},
			{"FULLDUPLEX", duplexDetail(csma.FullDuplex), func() error { return c.radio.SetFullDuplex(op, csma.FullDuplex) }},
		}
		for _, setting := range settings {
			if err := setting.send(); err != nil {
				add(setting.command, setting.detail, stepResultWord(err))
				return c.recover(prev, radioSent, powerSent, csmaSent, steps, c.modemClassErr(setting.command, err))
			}
			add(setting.command, setting.detail, "sent_no_reply")
		}
	}

	// The final ping closes every change (API-MD-3).
	if err := c.radio.Ping(op); err != nil {
		add("Ping", "", stepResultWord(err))
		return c.recover(prev, radioSent, powerSent, csmaSent, steps, c.modemClassErr("Ping", err))
	}
	add("Ping", "", "pong")

	// Save: re-read kam.yml, refuse to overwrite a concurrent edit, keep every
	// non-radio value, and write atomically (§7.5.1 step 4).
	newSettings, err := c.mergeAndSave(fileBefore, wanted, txPower, csma)
	if err != nil {
		switch {
		case errors.Is(err, errConfigChanged):
			add("save", "", "conflict")
			failure := &api.ModemError{Class: api.ModemErrorConflict, Message: "kam.yml changed during the change; nothing was overwritten"}
			return c.recover(prev, radioSent, powerSent, csmaSent, steps, failure)
		default:
			add("save", "", "failed")
			failure := &api.ModemError{Class: api.ModemErrorInternal, Message: "could not save kam.yml", Cause: err}
			return c.recover(prev, radioSent, powerSent, csmaSent, steps, failure)
		}
	}
	add("save", "", "ok")

	// Publish the new runtime truth, then resume TX (§7.5.1 step 5). A
	// successful apply also clears a pause left by an earlier failure
	// (§7.5.1 step 7).
	c.setTxPaused(false)
	c.runtime.publish(&echo, *newSettings)
	c.manager.UpdateRadioSettings(*newSettings)
	c.setSignalReport(true)
	c.signalRefresh()
	verifiedSource := modemEchoSource
	result := api.ModemRadioResult{Applied: true, Saved: true, Verified: &verifiedSource, Restored: false, TxPaused: false, Steps: steps}
	c.setLastChange(&result)
	c.scheduler.Resume()
	slog.Info("radio settings applied",
		"frequency_hz", echo.Frequency, "bandwidth_hz", echo.Bandwidth,
		"spreading_factor", echo.SpreadingFactor, "coding_rate", echo.CodingRate,
		"saved", true, "steps", len(steps))
	return result, nil
}

const modemEchoSource = "modem_echo"

// errConfigChanged marks a kam.yml that was edited while the change ran.
var errConfigChanged = errors.New("config file changed during the change")

// mergeAndSave re-reads kam.yml, refuses when the file changed since the
// operation started, merges the new radio values into the fresh file
// (preserving port, baud, and every non-radio value), and saves atomically.
func (c *modemController) mergeAndSave(fileBefore []byte, wanted kiss.RadioConfig, txPower *uint8, csma *kiss.CSMAConfig) (*models.RadioConfig, error) {
	fresh, err := models.LoadKamConfig(c.configPath)
	if err != nil {
		return nil, fmt.Errorf("load kam.yml: %w", err)
	}
	fileAfter, err := os.ReadFile(c.configPath)
	if err != nil {
		return nil, fmt.Errorf("read kam.yml: %w", err)
	}
	if !bytes.Equal(fileBefore, fileAfter) {
		return nil, errConfigChanged
	}
	fresh.Radio.FrequencyHz = wanted.Frequency
	fresh.Radio.BandwidthHz = wanted.Bandwidth
	fresh.Radio.SpreadingFactor = wanted.SpreadingFactor
	fresh.Radio.CodingRate = wanted.CodingRate
	if txPower != nil {
		value := *txPower
		fresh.Radio.TxPowerDBm = &value
	}
	if csma != nil {
		fresh.Radio.CSMA = &models.CSMAConfig{
			TxDelayMS: csma.TxDelayMS, Persistence: int(csma.Persistence),
			SlotTimeMS: csma.SlotTimeMS, FullDuplex: csma.FullDuplex,
		}
	}
	if err := models.SaveKamConfig(c.configPath, fresh); err != nil {
		return nil, fmt.Errorf("save kam.yml: %w", err)
	}
	settings := fresh.Radio
	return &settings, nil
}

// capture reads the state a failed change must restore. Only a failed radio
// echo capture aborts the change; the other values simply restore to defaults.
func (c *modemController) capture(ctx context.Context) (previousSettings, error) {
	var prev previousSettings
	prev.csma = c.manager.CurrentCSMA()
	echo, err := c.radio.GetRadio(ctx)
	if err == nil {
		prev.echo = &echo
	} else {
		return prev, err
	}
	if power, err := c.radio.GetTxPower(ctx); err == nil {
		value := power
		prev.power = &value
	}
	if enabled, err := c.radio.GetSignalReport(ctx); err == nil {
		value := enabled
		prev.signal = &value
	}
	return prev, nil
}

// stepResultWord classifies a command failure into the step vocabulary.
func stepResultWord(err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, kiss.ErrModemUnresponsive) {
		return "timeout"
	}
	return "failed"
}

// recover undoes everything the change sent (§7.5.1 step 6), on its own 5 s
// budget, independent of the operation's context and of the HTTP request. It
// records one aggregate restore step, resumes TX when the restored state is
// confirmed, and returns the failure body for the mapped status.
func (c *modemController) recover(prev previousSettings, radioSent, powerSent, csmaSent bool, steps []api.ModemStep, failure *api.ModemError) (api.ModemRadioResult, error) {
	restoreCtx, cancel := context.WithTimeout(context.Background(), modemRestoreBudget)
	defer cancel()

	anyTimeout := false
	radioRestored := false
	restorable := prev.echo != nil
	if radioSent && restorable {
		if err := c.radio.SetRadio(restoreCtx, *prev.echo); err != nil {
			anyTimeout = anyTimeout || stepResultWord(err) == "timeout"
			slog.Error("radio restore could not set the previous radio", "error", err)
		} else if got, err := c.radio.GetRadio(restoreCtx); err != nil {
			anyTimeout = anyTimeout || stepResultWord(err) == "timeout"
			slog.Error("radio restore could not read the echo", "error", err)
		} else if got == *prev.echo {
			radioRestored = true
		} else {
			slog.Error("radio restore echo mismatch", "got", radioDetail(got), "want", radioDetail(*prev.echo))
		}
	} else if radioSent {
		slog.Error("radio restore impossible: no previous radio echo was captured")
	}
	powerRestored := true
	if powerSent {
		if prev.power != nil {
			if err := c.radio.SetTxPower(restoreCtx, *prev.power); err != nil {
				anyTimeout = anyTimeout || stepResultWord(err) == "timeout"
				powerRestored = false
				slog.Error("radio restore could not set the previous TX power", "error", err)
			}
		} else {
			powerRestored = false
			slog.Error("radio restore impossible: no previous TX power was captured")
		}
	}
	signal := true
	if prev.signal != nil {
		signal = *prev.signal
	}
	restoredSignal, err := c.radio.SetSignalReport(restoreCtx, signal)
	if err != nil {
		anyTimeout = anyTimeout || stepResultWord(err) == "timeout"
		slog.Error("radio restore could not set signal reporting", "error", err)
	} else {
		c.setSignalReport(restoredSignal)
	}
	csmaRestored := true
	if csmaSent {
		sent := []struct {
			name string
			send func() error
		}{
			{"TXDELAY", func() error { return c.radio.SetTxDelay(restoreCtx, prev.csma.TxDelayMS) }},
			{"PERSISTENCE", func() error { return c.radio.SetPersistence(restoreCtx, prev.csma.Persistence) }},
			{"SLOTTIME", func() error { return c.radio.SetSlotTime(restoreCtx, prev.csma.SlotTimeMS) }},
			{"FULLDUPLEX", func() error { return c.radio.SetFullDuplex(restoreCtx, prev.csma.FullDuplex) }},
		}
		for _, setter := range sent {
			if err := setter.send(); err != nil {
				anyTimeout = anyTimeout || stepResultWord(err) == "timeout"
				csmaRestored = false
				slog.Error("radio restore could not send CSMA value", "command", setter.name, "error", err)
			}
		}
	}

	// A confirmed radio echo plus sent power and CSMA settings permits TX to
	// resume (§7.5.1 step 6); otherwise the state stays uncertain and TX
	// stays paused while RX continues (step 7).
	resume := radioRestored && powerRestored && csmaRestored && !anyTimeout
	if resume {
		c.scheduler.Resume()
	}
	c.setTxPaused(!resume)

	aggregate := "sent_no_reply"
	switch {
	case anyTimeout:
		aggregate = "timeout"
	case radioRestored:
		aggregate = "echo_matches"
	case radioSent && restorable:
		aggregate = "failed"
	}
	steps = append(steps, api.ModemStep{Command: "restore", Result: aggregate})

	var verified *string
	if radioRestored {
		source := modemEchoSource
		verified = &source
	}
	result := api.ModemRadioResult{
		Error: failure.Message, Applied: false, Saved: false,
		Verified: verified, Restored: resume, TxPaused: !resume, Steps: steps,
	}
	c.setLastChange(&result)
	stored := result
	slog.Error("radio change failed", "error", failure.Message, "restore", aggregate, "tx_paused", !resume)
	return result, &api.ModemError{Class: failure.Class, Message: failure.Message, Cause: failure.Cause, Result: &stored}
}

// pausedState reports the pause flag for the apply's deferred resume guard.
func (c *modemController) pausedState() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.txPaused
}

// failResult is the shape for failures that never touched the modem: no
// restore happened. The apply's deferred guard resumes TX unless a pause from
// an earlier failed change is in effect.
func failResult(c *modemController, result api.ModemRadioResult, class api.ModemErrorClass, message string) (api.ModemRadioResult, error) {
	result.TxPaused = c.pausedState()
	c.setLastChange(&result)
	stored := result
	return result, &api.ModemError{Class: class, Message: message, Result: &stored}
}
