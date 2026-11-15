// The script routes driven over the unified listener with the real adapter
// and runtime: this is where the wire shapes, the adapter's translation and
// the capability's policy meet. See docs/FeatureUnifiedApi.md.

package node

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"kam.bytecoder.nl/internal/api"
	"kam.bytecoder.nl/internal/automation"
	"kam.bytecoder.nl/internal/db"
	"kam.bytecoder.nl/internal/events"
)

type stubSender struct{}

func (stubSender) Send(context.Context, string, string) error { return nil }

func scriptServer(t *testing.T) (*httptest.Server, *automation.Runtime) {
	t.Helper()
	repository, err := db.OpenAutomationRepository(filepath.Join(t.TempDir(), "automation.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	runtime, err := automation.NewRuntime(context.Background(), repository, stubSender{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.Stop)
	handler := api.NewHandlerWithResources(apiEvents{events.NewRing(16)}, func() api.Status { return api.Status{} }, apiScripts{runtime: runtime}, nil, nil, nil, nil)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server, runtime
}

func doScript(t *testing.T, method, url, body, contentType string) (int, string) {
	t.Helper()
	var request *http.Request
	var err error
	if body != "" {
		request, err = http.NewRequest(method, url, strings.NewReader(body))
	} else {
		request, err = http.NewRequest(method, url, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	text, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(text)
}

func scriptPath(server *httptest.Server, suffix string) string {
	return server.URL + suffix
}

func TestScriptRoutesThroughUnifiedListener(t *testing.T) {
	server, _ := scriptServer(t)

	// A create without timeout_ms applies the capability's default (D7).
	status, body := doScript(t, http.MethodPost, scriptPath(server, "/scripts"),
		`{"script":{"id":"one","name":"one","source":"def on_group_text(message):\n    pass\n"}}`, "application/json")
	if status != http.StatusCreated {
		t.Fatalf("create: %d %s", status, body)
	}
	var created db.AutomationScript
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID != "one" || created.Name != "one" || created.Enabled || created.TimeoutMS != 500 || created.RuntimeStatus != "READY" || created.Revision != 1 || created.LastError != "" {
		t.Fatalf("created script: %+v", created)
	}

	// Duplicate name and stale revision are conflicts with the capability's text.
	status, body = doScript(t, http.MethodPost, scriptPath(server, "/scripts"),
		`{"script":{"id":"two","name":"one","source":"def on_group_text(message):\n    pass\n"}}`, "application/json")
	if status != http.StatusConflict || body != `{"error":"automation script name already in use"}` {
		t.Fatalf("duplicate name: %d %q", status, body)
	}
	update := `{"expected_revision":99,"script":{"name":"changed","source":"def on_group_text(message):\n    pass\n","enabled":true,"timeout_ms":250}}`
	status, body = doScript(t, http.MethodPut, scriptPath(server, "/scripts/one"), update, "application/json")
	if status != http.StatusConflict || body != `{"error":"automation script revision conflict"}` {
		t.Fatalf("stale revision: %d %q", status, body)
	}

	// A fresh revision updates, then reads back through the same listener.
	fresh := `{"expected_revision":1,"script":{"name":"changed","source":"def on_group_text(message):\n    pass\n","enabled":true,"timeout_ms":250}}`
	status, body = doScript(t, http.MethodPut, scriptPath(server, "/scripts/one"), fresh, "application/json")
	if status != http.StatusOK {
		t.Fatalf("update: %d %s", status, body)
	}
	var updated db.AutomationScript
	if err := json.Unmarshal([]byte(body), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Revision != 2 || updated.Name != "changed" || updated.TimeoutMS != 250 || !updated.Enabled {
		t.Fatalf("updated script: %+v", updated)
	}
	status, body = doScript(t, http.MethodGet, scriptPath(server, "/scripts/one"), "", "")
	if status != http.StatusOK || !strings.Contains(body, `"name":"changed"`) {
		t.Fatalf("read: %d %q", status, body)
	}
	status, body = doScript(t, http.MethodGet, scriptPath(server, "/scripts"), "", "")
	if status != http.StatusOK {
		t.Fatalf("list: %d %q", status, body)
	}
	var listed []db.AutomationScript
	if err := json.Unmarshal([]byte(body), &listed); err != nil || len(listed) != 1 {
		t.Fatalf("list: %s %v %v", body, listed, err)
	}

	// The timer interval comes from the compiled source, is visible on reads, and a
	// request cannot set it.
	status, body = doScript(t, http.MethodPost, scriptPath(server, "/scripts"),
		`{"script":{"id":"timer","name":"timer","source":"timer_interval_minutes = 5\ndef on_timer(message):\n    pass\n","timer_interval_minutes":99}}`, "application/json")
	if status != http.StatusCreated || !strings.Contains(body, `"timer_interval_minutes":5`) {
		t.Fatalf("timer create: %d %q", status, body)
	}
	status, body = doScript(t, http.MethodGet, scriptPath(server, "/scripts/timer"), "", "")
	if status != http.StatusOK || !strings.Contains(body, `"timer_interval_minutes":5`) {
		t.Fatalf("timer read: %d %q", status, body)
	}
	status, body = doScript(t, http.MethodGet, scriptPath(server, "/scripts"), "", "")
	if status != http.StatusOK || !strings.Contains(body, `"timer_interval_minutes":5`) || !strings.Contains(body, `"timer_interval_minutes":0`) {
		t.Fatalf("timer list: %d %q", status, body)
	}

	// Deleting pins a revision; a missing script is a 404 with the capability text.
	status, body = doScript(t, http.MethodDelete, scriptPath(server, "/scripts/one?revision=2"), "", "")
	if status != http.StatusNoContent || body != "" {
		t.Fatalf("delete: %d %q", status, body)
	}
	status, body = doScript(t, http.MethodGet, scriptPath(server, "/scripts/one"), "", "")
	if status != http.StatusNotFound || body != `{"error":"automation script not found"}` {
		t.Fatalf("read after delete: %d %q", status, body)
	}

	// Validation succeeds and compiles nothing in.
	status, body = doScript(t, http.MethodPost, scriptPath(server, "/scripts/validate"),
		`{"source":"def on_group_text(message):\n    pass\n"}`, "application/json")
	if status != http.StatusOK || body != `{"valid":true}` {
		t.Fatalf("validate: %d %q", status, body)
	}
	status, body = doScript(t, http.MethodPost, scriptPath(server, "/scripts/validate"),
		`{"source":"def on_group_text(message:\n"}`, "application/json")
	if status != http.StatusUnprocessableEntity || body == "" {
		t.Fatalf("invalid validate: %d %q", status, body)
	}
}

func TestListenerPolicyCoversScriptRoutes(t *testing.T) {
	server, _ := scriptServer(t)

	// Non-JSON bodies are refused before any capability work happens.
	status, body := doScript(t, http.MethodPost, scriptPath(server, "/scripts"),
		`{"script":{"id":"form"}}`, "text/plain")
	if status != http.StatusUnsupportedMediaType || body != `{"error":"Content-Type must be application/json"}` {
		t.Fatalf("text/plain: %d %q", status, body)
	}

	// A foreign Host header is refused.
	request, err := http.NewRequest(http.MethodGet, scriptPath(server, "/scripts"), nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "attacker.example"
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	bodyBytes, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden || strings.TrimSpace(string(bodyBytes)) != `{"error":"local API serves loopback hosts only"}` {
		t.Fatalf("foreign Host: %d %q", response.StatusCode, bodyBytes)
	}

	// Unknown fields in script bodies are rejected.
	status, body = doScript(t, http.MethodPost, scriptPath(server, "/scripts"),
		`{"script":{"id":"x"},"bogus":1}`, "application/json")
	if status != http.StatusBadRequest {
		t.Fatalf("unknown field: %d %q", status, body)
	}
}
