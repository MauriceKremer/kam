package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeModemSource records which seam method a route called and answers from a
// script.
type fakeModemSource struct {
	snapshot     ModemSnapshot
	queryResult  ModemQueryResult
	queryErr     error
	previewSteps []ModemStep
	previewErr   error
	applyResult  ModemRadioResult
	applyErr     error

	applied string // one of "", "query", "preview", "apply"
	delay   time.Duration
}

func (m *fakeModemSource) Snapshot(ctx context.Context) (ModemSnapshot, error) {
	m.applied = "snapshot"
	m.sleep(ctx)
	return m.snapshot, nil
}

func (m *fakeModemSource) Query(ctx context.Context, request ModemQueryRequest) (ModemQueryResult, error) {
	m.applied = "query"
	m.sleep(ctx)
	return m.queryResult, m.queryErr
}

func (m *fakeModemSource) Preview(ctx context.Context, request ModemRadioRequest) ([]ModemStep, error) {
	m.applied = "preview"
	m.sleep(ctx)
	return m.previewSteps, m.previewErr
}

func (m *fakeModemSource) Apply(ctx context.Context, request ModemRadioRequest) (ModemRadioResult, error) {
	m.applied = "apply"
	m.sleep(ctx)
	return m.applyResult, m.applyErr
}

func (m *fakeModemSource) sleep(ctx context.Context) {
	if m.delay == 0 {
		return
	}
	timer := time.NewTimer(m.delay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

// modemHandler builds a handler with a modem source (nil for the disabled
// routes).
func modemHandler(modem ModemSource) http.Handler {
	return NewHandlerWithResources(newRingSource(4), func() Status { return Status{} }, nil, nil, nil, modem, nil)
}

func postJSON(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "http://127.0.0.1"+path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestModemIndexAndDisabledRoutes(t *testing.T) {
	handler := modemHandler(nil)
	request := httptest.NewRequest("GET", "http://127.0.0.1/", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var index struct {
		Endpoints []apiEndpoint `json:"endpoints"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &index); err != nil {
		t.Fatal(err)
	}
	base := map[string]bool{}
	for _, item := range index.Endpoints {
		base[item.Method+" "+item.Path] = true
	}
	for _, want := range []string{
		"GET /v1/modem", "POST /v1/modem/query", "PUT /v1/modem/radio", "PUT /v1/modem/radio?preview=true",
	} {
		if !base[want] {
			t.Fatalf("index missing %s: %+v", want, base)
		}
	}
	snapshot := postJSON(handler, "GET", "/v1/modem", "")
	if snapshot.Code != http.StatusServiceUnavailable || snapshot.Body.String() != `{"error":"modem is disabled"}` {
		t.Fatalf("GET /v1/modem: %d %s", snapshot.Code, snapshot.Body)
	}
	if got := postJSON(handler, "POST", "/v1/modem/query", `{"command":"Ping"}`); got.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /v1/modem/query: %d %s", got.Code, got.Body)
	}
	if got := postJSON(handler, "PUT", "/v1/modem/radio", `{"frequency_hz":868000000}`); got.Code != http.StatusServiceUnavailable {
		t.Fatalf("PUT /v1/modem/radio: %d %s", got.Code, got.Body)
	}
	if got := postJSON(handler, "PUT", "/v1/modem/radio?preview=true", `{}`); got.Code != http.StatusServiceUnavailable {
		t.Fatalf("PUT /v1/modem/radio?preview=true: %d %s", got.Code, got.Body)
	}
}

// The documented examples stay parseable into the wire shapes.
func TestModemWireShapesParseTheDocumentedExamples(t *testing.T) {
	snapshotJSON := `{
	  "at":"2026-10-11T15:00:00.000Z",
	  "link":"up",
	  "device_name":"Emmen KISS",
	  "firmware_version":1,
	  "radio":{"frequency_hz":868000000,"bandwidth_hz":125000,"spreading_factor":8,"coding_rate":5,"tx_power_dbm":14,"source":"modem_echo"},
	  "signal_report":true,
	  "csma":{"txdelay_ms":500,"persistence":63,"slot_time_ms":100,"full_duplex":false},
	  "readings":{"rssi_dbm":-106,"noise_floor_dbm":-114,"noise_floor_calibrated":true,"channel_busy":false,"mcu_temp_c":32.4,"battery_mv":null,"stats":{"rx":1246,"tx":38,"errors":0}},
	  "unsupported":["GetBattery"],
	  "errors":{},
	  "tx_paused":false,
	  "last_change":null
	}`
	var snapshot ModemSnapshot
	if err := json.Unmarshal([]byte(snapshotJSON), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Link != "up" || snapshot.DeviceName == nil || *snapshot.DeviceName != "Emmen KISS" {
		t.Fatalf("snapshot: %+v", snapshot)
	}
	if snapshot.Readings == nil || snapshot.Readings.RSSIDBm == nil || *snapshot.Readings.RSSIDBm != -106 || snapshot.Readings.BatteryMV != nil {
		t.Fatalf("readings: %+v", snapshot.Readings)
	}
	if snapshot.Readings.Stats == nil || snapshot.Readings.Stats.RX != 1246 || snapshot.Readings.MCUTempC == nil || *snapshot.Readings.MCUTempC != 32.4 || snapshot.Readings.NoiseFloorCalibrated == false {
		t.Fatalf("readings: %+v", snapshot.Readings)
	}

	var query1 ModemQueryResult
	if err := json.Unmarshal([]byte(`{"command":"GetCurrentRssi","at":"2026-10-11T15:00:00.000Z","duration_ms":8,"supported":true,"value":-106}`), &query1); err != nil {
		t.Fatal(err)
	}
	if query1.Command != "GetCurrentRssi" || query1.DurationMS != 8 || !query1.Supported || query1.Value == nil {
		t.Fatalf("query: %+v", query1)
	}
	var query2 ModemQueryResult
	if err := json.Unmarshal([]byte(`{"command":"GetBattery","at":"2026-10-11T15:00:00.000Z","duration_ms":8,"supported":false,"value":null,"reason":"NoCallback"}`), &query2); err != nil {
		t.Fatal(err)
	}
	if query2.Supported || query2.Reason != "NoCallback" || query2.Value != nil {
		t.Fatalf("unsupported query: %+v", query2)
	}

	successJSON := `{
	  "applied": true, "saved": true, "verified": "modem_echo", "restored": false, "tx_paused": false,
	  "steps": [
	    { "command": "SetRadio", "detail": "868000000 Hz, 125000 Hz, SF8, CR5", "result": "ok" },
	    { "command": "GetRadio", "result": "echo_matches" },
	    { "command": "SetTxPower", "detail": "14 dBm", "result": "ok" },
	    { "command": "TXDELAY", "detail": "500 ms", "result": "sent_no_reply" },
	    { "command": "Ping", "result": "pong" }
	  ]
	}`
	var success ModemRadioResult
	if err := json.Unmarshal([]byte(successJSON), &success); err != nil {
		t.Fatal(err)
	}
	if !success.Applied || !success.Saved || success.Verified == nil || *success.Verified != "modem_echo" || success.TxPaused || len(success.Steps) != 5 {
		t.Fatalf("success result: %+v", success)
	}
	var failure ModemRadioResult
	if err := json.Unmarshal([]byte(`{"error":"Apply and restoration failed","applied":false,"saved":false,"restored":false,"tx_paused":true,"verified":null,"steps":[{"command":"SetRadio","result":"timeout"},{"command":"restore","result":"timeout"}]}`), &failure); err != nil {
		t.Fatal(err)
	}
	if failure.Applied || failure.Restored || !failure.TxPaused || failure.Verified != nil || failure.Error == "" || len(failure.Steps) != 2 {
		t.Fatalf("failure result: %+v", failure)
	}
}

func TestModemSnapshotServedFromCache(t *testing.T) {
	source := &fakeModemSource{snapshot: ModemSnapshot{Link: "up", Radio: ModemRadioStatus{Source: "modem_echo"}, Unsupported: []string{}, Errors: map[string]string{}}}
	snapshot := postJSON(modemHandler(source), "GET", "/v1/modem", "")
	if snapshot.Code != http.StatusOK {
		t.Fatalf("status %d: %s", snapshot.Code, snapshot.Body)
	}
	if source.applied != "snapshot" {
		t.Fatal("the snapshot route did not reach the source")
	}
	var value ModemSnapshot
	if err := json.Unmarshal(snapshot.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if value.Link != "up" || len(value.Unsupported) != 0 || value.Errors == nil || value.Radio.Source != "modem_echo" {
		t.Fatalf("snapshot: %s", snapshot.Body)
	}
}

func TestModemQueryDispatchAndErrors(t *testing.T) {
	source := &fakeModemSource{queryResult: ModemQueryResult{Command: "Ping", Supported: true, Value: "pong"}}
	got := postJSON(modemHandler(source), "POST", "/v1/modem/query", `{"command":"Ping"}`)
	if got.Code != http.StatusOK || source.applied != "query" || source.applied == "" {
		t.Fatalf("status %d: %s", got.Code, got.Body)
	}
	var result ModemQueryResult
	if err := json.Unmarshal(got.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Command != "Ping" || !result.Supported || result.Value != "pong" {
		t.Fatalf("result: %+v", result)
	}

	// An unknown command is the capability's 400; transport classes map onto
	// their statuses; an unclassified failure is 422.
	cases := []struct {
		code int
		err  error
	}{
		{400, &ModemError{Class: ModemErrorBadRequest, Message: "unknown command"}},
		{422, &ModemError{Class: ModemErrorInvalid, Message: "length is required"}},
		{409, &ModemError{Class: ModemErrorConflict, Message: "a radio change is in progress"}},
		{502, &ModemError{Class: ModemErrorRejected, Message: "modem rejected"}},
		{503, &ModemError{Class: ModemErrorLinkDown, Message: "link down"}},
		{504, &ModemError{Class: ModemErrorTimeout, Message: "timeout"}},
		{500, &ModemError{Class: ModemErrorInternal, Message: "internal"}},
		{422, errors.New("unclassified")},
	}
	for _, want := range cases {
		failing := &fakeModemSource{queryErr: want.err}
		got := postJSON(modemHandler(failing), "POST", "/v1/modem/query", `{"command":"Ping"}`)
		if got.Code != want.code {
			t.Fatalf("query error class %d: status %d: %s", want.code, got.Code, got.Body)
		}
		if !strings.HasPrefix(got.Body.String(), `{"error":`) {
			t.Fatalf("query error: body %s", got.Body)
		}
	}
}

func TestModemQueryStrictDecoding(t *testing.T) {
	for _, body := range []string{
		`{"command":"Ping","perm":1}`, // unknown field
		`{"command":"Ping","length":5,"perm":1}`,
	} {
		got := postJSON(modemHandler(&fakeModemSource{}), "POST", "/v1/modem/query", body)
		if got.Code != http.StatusBadRequest {
			t.Fatalf("query %s: status %d: %s", body, got.Code, got.Body)
		}
	}
}

func TestModemRadioApplyRoute(t *testing.T) {
	verified := "modem_echo"
	source := &fakeModemSource{applyResult: ModemRadioResult{Applied: true, Saved: true, Verified: &verified, Steps: []ModemStep{}}}
	got := postJSON(modemHandler(source), "PUT", "/v1/modem/radio", `{"frequency_hz":868000000,"bandwidth_hz":125000,"spreading_factor":8,"coding_rate":5}`)
	if got.Code != http.StatusOK {
		t.Fatalf("status %d: %s", got.Code, got.Body)
	}
	var result ModemRadioResult
	if err := json.Unmarshal(got.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Applied || !result.Saved || result.Verified == nil || *result.Verified != "modem_echo" {
		t.Fatalf("result: %+v", result)
	}

	// A failed apply with a recorded result answers the result at the mapped
	// status (the §7.5.1 failure examples); a plain failure the error object.
	cases := []struct {
		class ModemErrorClass
		code  int
	}{
		{ModemErrorTimeout, http.StatusGatewayTimeout},
		{ModemErrorRejected, http.StatusBadGateway},
		{ModemErrorInternal, http.StatusInternalServerError},
		{ModemErrorConflict, http.StatusConflict},
	}
	for _, want := range cases {
		failing := &fakeModemSource{applyErr: &ModemError{Class: want.class, Message: "boom", Result: &ModemRadioResult{Error: "boom", Restored: true, Steps: []ModemStep{}}}}
		got := postJSON(modemHandler(failing), "PUT", "/v1/modem/radio", `{"frequency_hz":868000000,"bandwidth_hz":125000,"spreading_factor":8,"coding_rate":5}`)
		if got.Code != want.code || !strings.Contains(got.Body.String(), `"restored":true`) {
			t.Fatalf("apply failure class %d: status %d: %s", want.class, got.Code, got.Body)
		}
	}
	failing := &fakeModemSource{applyErr: &ModemError{Class: ModemErrorConflict, Message: "busy"}}
	got = postJSON(modemHandler(failing), "PUT", "/v1/modem/radio", `{"frequency_hz":868000000,"bandwidth_hz":125000,"spreading_factor":8,"coding_rate":5}`)
	if got.Code != http.StatusConflict || got.Body.String() != `{"error":"busy"}` {
		t.Fatalf("plain apply failure: %d %s", got.Code, got.Body)
	}
}

func TestModemRadioPreviewAndUnknownFields(t *testing.T) {
	source := &fakeModemSource{previewSteps: []ModemStep{{Command: "SetRadio"}}}
	got := postJSON(modemHandler(source), "PUT", "/v1/modem/radio?preview=true", `{"frequency_hz":868000000,"bandwidth_hz":125000,"spreading_factor":8,"coding_rate":5}`)
	if got.Code != http.StatusOK || source.applied != "preview" {
		t.Fatalf("status %d: %s", got.Code, got.Body)
	}
	var preview struct {
		Preview bool        `json:"preview"`
		Steps   []ModemStep `json:"steps"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if !preview.Preview || len(preview.Steps) != 1 || preview.Steps[0].Command != "SetRadio" {
		t.Fatalf("preview: %s", got.Body)
	}

	if got := postJSON(modemHandler(source), "PUT", "/v1/modem/radio?preview=false", `{}`); got.Code != http.StatusBadRequest {
		t.Fatalf("preview=false: %d %s", got.Code, got.Body)
	}
	if got := postJSON(modemHandler(source), "PUT", "/v1/modem/radio?preview=1", `{}`); got.Code != http.StatusBadRequest {
		t.Fatalf("preview=1: %d %s", got.Code, got.Body)
	}
	// signal_report is not an accepted field (rejected with 400); the
	// preview-failure path maps through the modem error classes.
	for _, body := range []string{
		`{"frequency_hz":868000000,"bandwidth_hz":125000,"spreading_factor":8,"coding_rate":5,"signal_report":true}`,
	} {
		got := postJSON(modemHandler(source), "PUT", "/v1/modem/radio", body)
		if got.Code != http.StatusBadRequest {
			t.Fatalf("unknown field %s: %d %s", body, got.Code, got.Body)
		}
	}
	failing := &fakeModemSource{previewErr: &ModemError{Class: ModemErrorInvalid, Message: "frequency_hz is required"}}
	got = postJSON(modemHandler(failing), "PUT", "/v1/modem/radio?preview=true", `{}`)
	if got.Code != http.StatusUnprocessableEntity || !strings.Contains(got.Body.String(), "frequency_hz is required") {
		t.Fatalf("preview validation: %d %s", got.Code, got.Body)
	}
}

// The radio apply route runs on the extended budget: anything beyond it times
// out there while ordinary routes keep the 5 s long deadline.
func TestModemRadioApplyDeadlineTier(t *testing.T) {
	saved := modemApplyTimeout
	defer func() { modemApplyTimeout = saved }()

	modemApplyTimeout = 120 * time.Millisecond
	long := &fakeModemSource{applyResult: ModemRadioResult{Applied: true}, delay: 200 * time.Millisecond}
	got := postJSON(modemHandler(long), "PUT", "/v1/modem/radio", `{"frequency_hz":868000000,"bandwidth_hz":125000,"spreading_factor":8,"coding_rate":5}`)
	if got.Code != http.StatusServiceUnavailable || !strings.Contains(got.Body.String(), "timed out") {
		t.Fatalf("apply beyond the extended budget: %d %s", got.Code, got.Body)
	}

	// The same slower answer on an ordinary route is far under its 5 s budget
	// and succeeds.
	other := &fakeModemSource{snapshot: ModemSnapshot{Link: "up"}, delay: 200 * time.Millisecond}
	got = postJSON(modemHandler(other), "GET", "/v1/modem", "")
	if got.Code != http.StatusOK {
		t.Fatalf("snapshot beyond 200 ms must still succeed under the 5 s budget: %d %s", got.Code, got.Body)
	}
}
