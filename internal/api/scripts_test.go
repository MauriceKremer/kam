package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// scriptFake records the seam's invocations and scripts each answer, so the
// tests below check what the handler forwards and how it maps errors to
// status codes — never what a real capability does.
type scriptFake struct {
	list    func() ([]Script, error)
	get     func(string) (Script, error)
	save    func(Script, int64) (Script, error)
	del     func(string, int64) error
	enabled func(string, bool, int64) (Script, error)
	recover func(string, int64, int64) (Script, error)
	prepare func(string, string, int64) error

	record []string
}

func (f *scriptFake) List(context.Context) ([]Script, error) {
	f.record = append(f.record, "list")
	if f.list == nil {
		return nil, nil
	}
	return f.list()
}

func (f *scriptFake) Get(_ context.Context, id string) (Script, error) {
	f.record = append(f.record, "get:"+id)
	if f.get == nil {
		return Script{}, nil
	}
	return f.get(id)
}

func (f *scriptFake) Save(_ context.Context, script Script, expectedRevision int64) (Script, error) {
	f.record = append(f.record, fmt.Sprintf("save:%s@%d", script.ID, expectedRevision))
	if f.save == nil {
		return script, nil
	}
	return f.save(script, expectedRevision)
}

func (f *scriptFake) Delete(_ context.Context, id string, revision int64) error {
	f.record = append(f.record, "delete:"+id)
	if f.del == nil {
		return nil
	}
	return f.del(id, revision)
}

func (f *scriptFake) SetEnabled(_ context.Context, id string, enabled bool, expectedRevision int64) (Script, error) {
	f.record = append(f.record, fmt.Sprintf("set:%s=%v@%d", id, enabled, expectedRevision))
	if f.enabled == nil {
		return Script{}, nil
	}
	return f.enabled(id, enabled, expectedRevision)
}

func (f *scriptFake) Recover(_ context.Context, id string, timeoutMS int64, expectedRevision int64) (Script, error) {
	f.record = append(f.record, fmt.Sprintf("recover:%s@%d+%d", id, timeoutMS, expectedRevision))
	if f.recover == nil {
		return Script{}, nil
	}
	return f.recover(id, timeoutMS, expectedRevision)
}

func (f *scriptFake) Validate(_ context.Context, id, source string, timeoutMS int64) error {
	f.record = append(f.record, fmt.Sprintf("validate:%s %q %d", id, source, timeoutMS))
	if f.prepare == nil {
		return nil
	}
	return f.prepare(id, source, timeoutMS)
}

func scriptHandler(scripts Scripts) http.Handler {
	return NewHandlerWithResources(newRingSource(8), func() Status { return Status{} }, scripts, nil, nil, nil, nil)
}

func scriptRequest(handler http.Handler, method, target, body, contentType string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Host = "127.0.0.1"
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestScriptRoutesAnswerDisabledWithoutAutomation(t *testing.T) {
	handler := scriptHandler(nil)
	for _, call := range []struct{ method, target, body string }{
		{http.MethodGet, "/scripts", ""},
		{http.MethodPost, "/scripts", `{"script":{"id":"x"}}`},
		{http.MethodGet, "/scripts/x", ""},
		{http.MethodPut, "/scripts/x", `{"script":{}}`},
		{http.MethodDelete, "/scripts/x?revision=1", ""},
		{http.MethodPost, "/scripts/validate", `{"source":"def on_group_text(message):\n    pass\n"}`},
		{http.MethodPost, "/scripts/x/enable", `{"expected_revision":1}`},
		{http.MethodPost, "/scripts/x/disable", `{"expected_revision":1}`},
		{http.MethodPost, "/scripts/x/recover", `{"expected_revision":1}`},
	} {
		response := scriptRequest(handler, call.method, call.target, call.body, "application/json")
		if response.Code != http.StatusServiceUnavailable || response.Body.String() != `{"error":"automation is disabled"}` {
			t.Fatalf("%s %s: got %d: %s", call.method, call.target, response.Code, response.Body)
		}
	}
}

func TestScriptRoutesForwardAndMapErrors(t *testing.T) {
	stored := Script{ID: "one", Name: "first", Source: "def on_group_text(message):\n    pass\n", Enabled: true, TimeoutMS: 500, RuntimeStatus: "READY", Revision: 3}
	var fake scriptFake
	var saved Script
	var savedRevision int64
	handler := scriptHandler(&fake)

	// An empty storage reads as an empty array, never null.
	fake.list = func() ([]Script, error) { return nil, nil }
	if response := scriptRequest(handler, http.MethodGet, "/scripts", "", ""); response.Code != http.StatusOK || response.Body.String() != "[]" {
		t.Fatalf("empty list: %d %q", response.Code, response.Body)
	}

	// A storage failure on list stays a server error with its text.
	fake.list = func() ([]Script, error) { return nil, errors.New("disk full") }
	if response := scriptRequest(handler, http.MethodGet, "/scripts", "", ""); response.Code != http.StatusInternalServerError || response.Body.String() != `{"error":"disk full"}` {
		t.Fatalf("failing list: %d %q", response.Code, response.Body)
	}
	fake.list = func() ([]Script, error) { return []Script{stored}, nil }

	// Create forwards the body and the create contract (revision 0), untouched.
	fake.save = func(script Script, expected int64) (Script, error) {
		saved, savedRevision = script, expected
		script.Revision, script.TimeoutMS = 1, 500
		return script, nil
	}
	response := scriptRequest(handler, http.MethodPost, "/scripts", `{"script":{"id":"c1","name":"n","source":"def on_group_text(message):\n    pass\n"}}`, "application/json")
	if response.Code != http.StatusCreated || saved.ID != "c1" || saved.TimeoutMS != 0 || savedRevision != 0 {
		t.Fatalf("create: %d saved %+v@%d", response.Code, saved, savedRevision)
	}

	// An update takes the id from the path.
	fake.save = func(script Script, expected int64) (Script, error) {
		saved, savedRevision = script, expected
		return script, nil
	}
	response = scriptRequest(handler, http.MethodPut, "/scripts/one?ignored=1", `{"expected_revision":5,"script":{"name":"changed","timeout_ms":250}}`, "application/json")
	if response.Code != http.StatusOK || saved.ID != "one" || saved.Name != "changed" || savedRevision != 5 {
		t.Fatalf("update: %d saved %+v@%d", response.Code, saved, savedRevision)
	}

	// The mutation routes forward their bodies verbatim.
	fake.enabled = func(id string, enabled bool, expected int64) (Script, error) { return stored, nil }
	if response := scriptRequest(handler, http.MethodPost, "/scripts/one/enable", `{"expected_revision":2}`, "application/json"); response.Code != http.StatusOK {
		t.Fatalf("enable: %d", response.Code)
	}
	fake.recover = func(id string, timeoutMS int64, expected int64) (Script, error) { return stored, nil }
	if response := scriptRequest(handler, http.MethodPost, "/scripts/one/recover", `{"expected_revision":2,"timeout_ms":250}`, "application/json"); response.Code != http.StatusOK {
		t.Fatalf("recover: %d", response.Code)
	}
	if fake.record[len(fake.record)-2] != "set:one=true@2" || fake.record[len(fake.record)-1] != "recover:one@250+2" {
		t.Fatalf("forwarded calls: %v", fake.record)
	}

	// Status mapping: classified errors carry their status and message.
	sentinel := func(class ScriptErrorClass) *ScriptError {
		return &ScriptError{Class: class, Message: "capability says no"}
	}
	fake.get = func(string) (Script, error) { return Script{}, sentinel(ScriptErrorNotFound) }
	response = scriptRequest(handler, http.MethodGet, "/scripts/one", "", "")
	if response.Code != http.StatusNotFound || response.Body.String() != `{"error":"capability says no"}` {
		t.Fatalf("not found: %d %q", response.Code, response.Body)
	}
	fake.save = func(Script, int64) (Script, error) { return Script{}, sentinel(ScriptErrorConflict) }
	response = scriptRequest(handler, http.MethodPut, "/scripts/one", `{"expected_revision":5,"script":{}}`, "application/json")
	if response.Code != http.StatusConflict {
		t.Fatalf("conflict: %d", response.Code)
	}

	// Unclassified failures are invalid.
	fake.get = func(string) (Script, error) { return Script{}, errors.New("compile failed") }
	if response := scriptRequest(handler, http.MethodGet, "/scripts/one", "", ""); response.Code != http.StatusUnprocessableEntity || response.Body.String() != `{"error":"compile failed"}` {
		t.Fatalf("invalid: %d %q", response.Code, response.Body)
	}
}

// The timer interval is part of the resource: it serializes on reads and a
// request value is carried to the capability untouched, which then ignores it.
func TestScriptCarriesTheTimerInterval(t *testing.T) {
	var fake scriptFake
	timer := Script{ID: "weather", Name: "weather", Source: "x", TimerIntervalMinutes: 5, Revision: 1}
	fake.list = func() ([]Script, error) { return []Script{timer}, nil }
	fake.get = func(string) (Script, error) { return timer, nil }
	handler := scriptHandler(&fake)

	response := scriptRequest(handler, http.MethodGet, "/scripts", "", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"timer_interval_minutes":5`) {
		t.Fatalf("list: %d %q", response.Code, response.Body)
	}
	response = scriptRequest(handler, http.MethodGet, "/scripts/weather", "", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"timer_interval_minutes":5`) {
		t.Fatalf("get: %d %q", response.Code, response.Body)
	}
	// The field is part of the accepted body, not an unknown field.
	var forwarded Script
	fake.save = func(script Script, expected int64) (Script, error) {
		forwarded = script
		return script, nil
	}
	response = scriptRequest(handler, http.MethodPost, "/scripts", `{"script":{"id":"weather","name":"weather","source":"x","timer_interval_minutes":5}}`, "application/json")
	if response.Code != http.StatusCreated || forwarded.TimerIntervalMinutes != 5 {
		t.Fatalf("create: %d %q, forwarded %d", response.Code, response.Body, forwarded.TimerIntervalMinutes)
	}
}

func TestScriptRouteBodiesAndValidation(t *testing.T) {
	var fake scriptFake
	var validatedID, validatedSource string
	var validatedTimeout int64
	fake.prepare = func(id, source string, timeoutMS int64) error {
		validatedID, validatedSource, validatedTimeout = id, source, timeoutMS
		return nil
	}
	handler := scriptHandler(&fake)

	// Validate forwards raw values and gives an empty id its default name.
	response := scriptRequest(handler, http.MethodPost, "/scripts/validate", `{"source":"def on_group_text(message):\n    pass\n"}`, "application/json")
	if response.Code != http.StatusOK || response.Body.String() != `{"valid":true}` {
		t.Fatalf("validate: %d %q", response.Code, response.Body)
	}
	if validatedID != "validation" || validatedTimeout != 0 || validatedSource == "" {
		t.Fatalf("validate forwarded: %q %d", validatedID, validatedTimeout)
	}

	// A validation failure keeps its message.
	fake.prepare = func(string, string, int64) error { return errors.New("timeout_ms must be positive") }
	if response := scriptRequest(handler, http.MethodPost, "/scripts/validate", `{"source":"x","timeout_ms":-1}`, "application/json"); response.Code != http.StatusUnprocessableEntity || response.Body.String() != `{"error":"timeout_ms must be positive"}` {
		t.Fatalf("failing validate: %d %q", response.Code, response.Body)
	}

	// Unknown fields and malformed bodies are rejected.
	for _, body := range []string{"{" + `"source":"x","bogus":1` + "}", "not json"} {
		if response := scriptRequest(handler, http.MethodPost, "/scripts/validate", body, "application/json"); response.Code != http.StatusBadRequest {
			t.Fatalf("bad body %q: %d", body, response.Code)
		}
	}

	// The mutation routes demand a positive revision.
	for _, target := range []string{"/scripts/x?revision=0", "/scripts/x", "/scripts/x?revision=no"} {
		if response := scriptRequest(handler, http.MethodDelete, target, "", ""); response.Code != http.StatusBadRequest || response.Body.String() != `{"error":"positive revision query parameter is required"}` {
			t.Fatalf("delete %s: %d %q", target, response.Code, response.Body)
		}
	}
	for _, target := range []string{"/scripts/x/enable", "/scripts/x/disable", "/scripts/x/recover"} {
		if response := scriptRequest(handler, http.MethodPost, target, "{\"expected_revision\":0}", "application/json"); response.Code != http.StatusBadRequest || response.Body.String() != `{"error":"positive expected_revision is required"}` {
			t.Fatalf("%s zero revision: %d %q", target, response.Code, response.Body)
		}
	}
}

func TestListenerGuardsScriptRoutes(t *testing.T) {
	var fake scriptFake
	fake.save = func(script Script, expectedRevision int64) (Script, error) {
		script.Revision = expectedRevision + 1
		return script, nil
	}
	server := httptest.NewServer(scriptHandler(&fake))
	defer server.Close()

	response, err := http.Post(server.URL+"/scripts", "application/json", strings.NewReader(`{"script":{"id":"wire","name":"wire","source":"def on_group_text(message):\n    pass\n"}}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create with charset JSON content type: %d", response.StatusCode)
	}

	form, err := http.NewRequest(http.MethodPost, server.URL+"/scripts", strings.NewReader(`{"script":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	form.Header.Set("Content-Type", "text/plain")
	response, err = server.Client().Do(form)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusUnsupportedMediaType || strings.TrimSpace(string(body)) != `{"error":"Content-Type must be application/json"}` {
		t.Fatalf("text/plain body: %d %q", response.StatusCode, body)
	}

	rebound, err := http.NewRequest(http.MethodGet, server.URL+"/scripts", nil)
	if err != nil {
		t.Fatal(err)
	}
	rebound.Host = "attacker.example"
	response, err = server.Client().Do(rebound)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden || strings.TrimSpace(string(body)) != `{"error":"local API serves loopback hosts only"}` {
		t.Fatalf("rebound Host: %d %q", response.StatusCode, body)
	}
}

func TestLoopbackHostGuard(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1:8766": true, "localhost:8766": true, "LOCALHOST": true, "[::1]:8766": true, "127.0.0.1": true,
		"attacker.example:8766": false, "192.168.1.5:8766": false, "0.0.0.0:8766": false, "127.0.0.1.attacker.example": false, "": false,
	} {
		if got := loopbackHost(host); got != want {
			t.Errorf("loopbackHost(%q): got %v, want %v", host, got, want)
		}
	}
}
