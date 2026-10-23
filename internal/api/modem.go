// Modem routes and the modem capability seam: a cached snapshot, one
// read-only command at a time, and radio changes with a preview. The handler
// is transport only; internal/node's adapter owns the cached state, the
// refresh cycle and the modem conversation (FeatureKamApi.md §7.5).

package api

import (
	"context"

	"errors"

	"net/http"

	"time"
)

// ModemRadioStatus reuses the /v1/status radio shape: the modem's cached echo
// of the host-set values, never a chip readback.
type ModemRadioStatus = RadioStatus

// ModemCSMA is the CSMA block of the snapshot: the configured KISS parameters.
type ModemCSMA struct {
	TxDelayMS   int  `json:"txdelay_ms"`
	Persistence int  `json:"persistence"`
	SlotTimeMS  int  `json:"slot_time_ms"`
	FullDuplex  bool `json:"full_duplex"`
}

// ModemStats is the modem's frame counters.
type ModemStats struct {
	RX     uint32 `json:"rx"`
	TX     uint32 `json:"tx"`
	Errors uint32 `json:"errors"`
}

// ModemReadings are the hardware readings the 10 s refresh collects. A nil
// pointer means the value is unknown: the command has not run yet, the board
// does not support it, or the last attempt failed.
type ModemReadings struct {
	RSSIDBm              *int8       `json:"rssi_dbm"`
	NoiseFloorDBm        *int16      `json:"noise_floor_dbm"`
	NoiseFloorCalibrated bool        `json:"noise_floor_calibrated"`
	ChannelBusy          *bool       `json:"channel_busy"`
	MCUTempC             *float64    `json:"mcu_temp_c"`
	BatteryMV            *uint16     `json:"battery_mv"`
	Stats                *ModemStats `json:"stats"`
}

// ModemStep is one planned or executed command of a radio change. Detail and
// result are absent in preview steps.
type ModemStep struct {
	Command string `json:"command"`
	Detail  string `json:"detail,omitempty"`
	Result  string `json:"result,omitempty"`
}

// ModemRadioResult reports a radio change. A failure body carries the same
// fields with error set; applied means the entire requested change succeeded,
// not merely that some commands reached the modem.
type ModemRadioResult struct {
	Error    string      `json:"error,omitempty"`
	Applied  bool        `json:"applied"`
	Saved    bool        `json:"saved"`
	Verified *string     `json:"verified"`
	Restored bool        `json:"restored"`
	TxPaused bool        `json:"tx_paused"`
	Steps    []ModemStep `json:"steps"`
}

// ModemSnapshot is the cached shape served at GET /v1/modem; the adapter
// refreshes it at most every 10 s and never talks to the modem in a request.
type ModemSnapshot struct {
	At              time.Time         `json:"at"`
	Link            string            `json:"link"`
	DeviceName      *string           `json:"device_name"`
	FirmwareVersion *uint8            `json:"firmware_version"`
	Radio           ModemRadioStatus  `json:"radio"`
	SignalReport    *bool             `json:"signal_report"`
	CSMA            ModemCSMA         `json:"csma"`
	Readings        *ModemReadings    `json:"readings"`
	Unsupported     []string          `json:"unsupported"`
	Errors          map[string]string `json:"errors"`
	TxPaused        bool              `json:"tx_paused"`
	LastChange      *ModemRadioResult `json:"last_change"`
}

// ModemQueryRequest is the body of POST /v1/modem/query. Length and
// permissions are decoded as floats so that non-integer or out-of-range
// values become semantic (422) errors in the capability instead of decode
// (400) errors here.
type ModemQueryRequest struct {
	Command     string   `json:"command"`
	Length      *float64 `json:"length,omitempty"`
	Permissions *float64 `json:"permissions,omitempty"`
}

// ModemQueryResult is the response of POST /v1/modem/query. An unsupported
// command is not an error: supported is false, value is null and reason names
// why.
type ModemQueryResult struct {
	Command    string    `json:"command"`
	At         time.Time `json:"at"`
	DurationMS int64     `json:"duration_ms"`
	Supported  bool      `json:"supported"`
	Value      any       `json:"value"`
	Reason     string    `json:"reason,omitempty"`
}

// ModemRadioValue is the radio tuple GetRadio returns.
type ModemRadioValue struct {
	FrequencyHz     uint32 `json:"frequency_hz"`
	BandwidthHz     uint32 `json:"bandwidth_hz"`
	SpreadingFactor uint8  `json:"spreading_factor"`
	CodingRate      uint8  `json:"coding_rate"`
}

// ModemVersionValue is the version tuple GetVersion returns.
type ModemVersionValue struct {
	Version  uint8 `json:"version"`
	Reserved uint8 `json:"reserved"`
}

// ModemSensorsValue is what GetSensors returns.
type ModemSensorsValue struct {
	Permissions int    `json:"permissions"`
	DataHex     string `json:"data_hex"`
}

// ModemRadioRequest is the body of PUT /v1/modem/radio. The four radio values
// are required; tx_power_dbm and csma are optional and leave the setting alone
// when omitted. signal_report is not an accepted field.
type ModemRadioRequest struct {
	FrequencyHz     *float64        `json:"frequency_hz"`
	BandwidthHz     *float64        `json:"bandwidth_hz"`
	SpreadingFactor *float64        `json:"spreading_factor"`
	CodingRate      *float64        `json:"coding_rate"`
	TxPowerDBm      *float64        `json:"tx_power_dbm,omitempty"`
	CSMA            *ModemRadioCSMA `json:"csma,omitempty"`
}

// ModemRadioCSMA is the CSMA block of a radio change. Fields are pointers so
// the capability can require the complete object; the shared model's YAML
// decoder fills defaults, the JSON API must not.
type ModemRadioCSMA struct {
	TxDelayMS   *float64 `json:"txdelay_ms"`
	Persistence *float64 `json:"persistence"`
	SlotTimeMS  *float64 `json:"slot_time_ms"`
	FullDuplex  *bool    `json:"full_duplex"`
}

// ModemSource is the modem capability seam: the snapshot seam, one read-only
// command, a validated preview and the guarded radio change. Implementations
// classify their failures with ModemError and run the change independent of
// the HTTP request context.
type ModemSource interface {
	Snapshot(ctx context.Context) (ModemSnapshot, error)
	Query(ctx context.Context, request ModemQueryRequest) (ModemQueryResult, error)
	Preview(ctx context.Context, request ModemRadioRequest) ([]ModemStep, error)
	Apply(ctx context.Context, request ModemRadioRequest) (ModemRadioResult, error)
}

// ModemErrorClass is the transport's error vocabulary: the status code a modem
// failure maps to. The capability decides the class, the handler decides the
// code. Unclassified failures are invalid (422).
type ModemErrorClass int

const (
	// ModemErrorInvalid is the default: a validation refusal (422).
	ModemErrorInvalid ModemErrorClass = iota
	// ModemErrorBadRequest maps to 400: an unknown command or field.
	ModemErrorBadRequest
	// ModemErrorConflict maps to 409: a concurrent change, a query during one,
	// or a kam.yml edit racing the save.
	ModemErrorConflict
	// ModemErrorRejected maps to 502: echo mismatch or modem rejection.
	ModemErrorRejected
	// ModemErrorLinkDown maps to 503: the modem link is lost.
	ModemErrorLinkDown
	// ModemErrorTimeout maps to 504: the modem did not answer in time.
	ModemErrorTimeout
	// ModemErrorInternal maps to 500: a KAM-side failure such as the
	// kam.yml write.
	ModemErrorInternal
)

// ModemError carries a modem failure to the handler. For failures after a
// modem command was sent, Result holds the full result body the handler
// returns at the class's status code.
type ModemError struct {
	Class   ModemErrorClass
	Message string
	Cause   error
	Result  *ModemRadioResult
}

func (e *ModemError) Error() string { return e.Message }
func (e *ModemError) Unwrap() error { return e.Cause }

// modemStatus maps an error class to its status code.
func modemStatus(class ModemErrorClass) int {
	switch class {
	case ModemErrorBadRequest:
		return http.StatusBadRequest
	case ModemErrorConflict:
		return http.StatusConflict
	case ModemErrorRejected:
		return http.StatusBadGateway
	case ModemErrorLinkDown:
		return http.StatusServiceUnavailable
	case ModemErrorTimeout:
		return http.StatusGatewayTimeout
	case ModemErrorInternal:
		return http.StatusInternalServerError
	default:
		return http.StatusUnprocessableEntity
	}
}

// writeModemError maps a modem failure to its status code; the body keeps the
// capability's own message.
func writeModemError(w http.ResponseWriter, err error) {
	var modemErr *ModemError
	class := ModemErrorInvalid
	if errors.As(err, &modemErr) {
		class = modemErr.Class
	}
	writeError(w, modemStatus(class), err)
}

// writeModemApplyError answers a failed change with its full result body at
// the mapped status when one was recorded, and a plain error otherwise.
func writeModemApplyError(w http.ResponseWriter, err error) {
	var modemErr *ModemError
	if errors.As(err, &modemErr) && modemErr.Result != nil {
		writeJSON(w, modemStatus(modemErr.Class), modemErr.Result)
		return
	}
	writeModemError(w, err)
}

// modemDisabled is the body the modem routes serve while no modem capability
// is wired: the routes stay registered and keep the index stable.
func modemDisabled(w http.ResponseWriter) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "modem is disabled"})
}

// registerModemRoutes registers the modem surface; it exists even when no
// ModemSource is wired, and answers 503 on every route in that case.
func registerModemRoutes(mux *http.ServeMux, modem ModemSource) {
	mux.HandleFunc("GET /v1/modem", func(w http.ResponseWriter, request *http.Request) {
		if modem == nil {
			modemDisabled(w)
			return
		}
		snapshot, err := modem.Snapshot(request.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, snapshot)
	})
	mux.HandleFunc("POST /v1/modem/query", func(w http.ResponseWriter, request *http.Request) {
		if modem == nil {
			modemDisabled(w)
			return
		}
		var body ModemQueryRequest
		if !decodeJSON(w, request, &body) {
			return
		}
		result, err := modem.Query(request.Context(), body)
		if err != nil {
			writeModemError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
	mux.HandleFunc("PUT /v1/modem/radio", func(w http.ResponseWriter, request *http.Request) {
		if modem == nil {
			modemDisabled(w)
			return
		}
		var body ModemRadioRequest
		if !decodeJSON(w, request, &body) {
			return
		}
		if preview := request.URL.Query().Get("preview"); preview != "" && preview != "true" {
			writeError(w, http.StatusBadRequest, errors.New("preview must be true"))
			return
		}
		if request.URL.Query().Has("preview") {
			steps, err := modem.Preview(request.Context(), body)
			if err != nil {
				writeModemError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"preview": true, "steps": steps})
			return
		}
		result, err := modem.Apply(request.Context(), body)
		if err != nil {
			writeModemApplyError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
}

// isModemRadioApply reports whether a request targets the one route with the

// longer deadline; the apply may legitimately take ~15 s (§7.5.1).

func isModemRadioApply(method, path string) bool {

	return method == http.MethodPut && path == "/v1/modem/radio"
}
