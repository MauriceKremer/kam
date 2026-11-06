package automation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.starlark.net/starlark"

	"kam.bytecoder.nl/internal/db"
	"kam.bytecoder.nl/internal/kiss/parsers"
)

// starQuote renders a Go string as a Starlark string literal.
func starQuote(s string) string { return strconv.Quote(s) }

// runHTTPCase saves source as a script, starts a runtime, feeds it one group
// text, and returns what the script replied and the script's status afterwards.
// The reply count is whatever arrived within the window; tests assert on it.
func runHTTPCase(t *testing.T, id, source string, timeoutMS int64, wait time.Duration) (*db.AutomationRepository, *recordingSender, *Runtime) {
	t.Helper()
	repository := openTestRepository(t)
	if _, err := repository.Save(context.Background(), db.AutomationScript{
		ID: id, Name: id, Source: source, Enabled: true, TimeoutMS: timeoutMS, RuntimeStatus: "READY",
	}, 0); err != nil {
		t.Fatalf("save: %v", err)
	}
	sender := &recordingSender{}
	runtime := newTestRuntime(t, repository, sender)
	runtime.Handle(parsers.GroupTextMessage{ChannelName: "public", Body: "go"})
	time.Sleep(wait)
	return repository, sender, runtime
}

func TestRestGetReturnsBodyAndNon2xxStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"nope"}`))
	}))
	defer server.Close()
	source := "def on_group_text(message):\n    body, status = rest_get(" + starQuote(server.URL) + ")\n    reply(message[\"channel\"], \"%d %s\" % (status, body))\n"
	_, sender, _ := runHTTPCase(t, "status", source, 5000, 200*time.Millisecond)
	got := sender.snapshot()
	if len(got) != 1 || got[0] != `503 {"error":"nope"}` {
		t.Fatalf("replies: got %v, want one `503 {\"error\":\"nope\"}`", got)
	}
}

func TestRestGetSendsHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.Header.Get("X-Api-Key") + "|" + r.Header.Get("Accept")))
	}))
	defer server.Close()
	source := "def on_group_text(message):\n    body, _ = rest_get(" + starQuote(server.URL) + ", headers={\"X-Api-Key\": \"secret\", \"Accept\": \"application/json\"})\n    reply(message[\"channel\"], body)\n"
	_, sender, _ := runHTTPCase(t, "headers", source, 5000, 200*time.Millisecond)
	got := sender.snapshot()
	if len(got) != 1 || got[0] != "secret|application/json" {
		t.Fatalf("replies: got %v, want `secret|application/json`", got)
	}
}

func TestRestGetRejectsBadHeaderDict(t *testing.T) {
	// Headers are read at call time, so the script compiles and fails on invocation.
	source := "def on_group_text(message):\n    rest_get(\"http://127.0.0.1:1/x\", headers={\"a\": 1})\n    reply(message[\"channel\"], \"unreachable\")\n"
	_, sender, runtime := runHTTPCase(t, "bad-headers", source, 500, 200*time.Millisecond)
	if got := sender.snapshot(); len(got) != 0 {
		t.Fatalf("script continued past a bad header dict: %v", got)
	}
	current, err := runtime.Get(context.Background(), "bad-headers")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if current.RuntimeStatus != "READY" {
		t.Fatalf("status: got %q, want READY (a script error must not freeze)", current.RuntimeStatus)
	}
}

func TestRestGetRejectsSchemeAndCredentials(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"ftp scheme", "ftp://example.com/x"},
		{"file scheme", "file:///etc/passwd"},
		{"credentials", "http://user:pass@example.com/x"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			// Reaching the network is proof the guard failed, so use a URL that
			// would resolve instantly if the guard let it through.
			source := "def on_group_text(message):\n    rest_get(" + starQuote(test.url) + ")\n    reply(message[\"channel\"], \"reached\")\n"
			_, sender, runtime := runHTTPCase(t, "guard", source, 500, 200*time.Millisecond)
			if got := sender.snapshot(); len(got) != 0 {
				t.Fatalf("reply after %s: %v", test.name, got)
			}
			current, err := runtime.Get(context.Background(), "guard")
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if current.RuntimeStatus != "READY" {
				t.Fatalf("status after %s: got %q, want READY", test.name, current.RuntimeStatus)
			}
		})
	}
}

func TestRestGetRejectsMalformedURL(t *testing.T) {
	source := "def on_group_text(message):\n    rest_get(\"http://[::1\")\n    reply(message[\"channel\"], \"reached\")\n"
	_, sender, runtime := runHTTPCase(t, "malformed", source, 500, 200*time.Millisecond)
	if got := sender.snapshot(); len(got) != 0 {
		t.Fatalf("reply after a malformed URL: %v", got)
	}
	current, err := runtime.Get(context.Background(), "malformed")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if current.RuntimeStatus != "READY" {
		t.Fatalf("status: got %q, want READY", current.RuntimeStatus)
	}
}

func TestRestGetBodyAtCapIsAccepted(t *testing.T) {
	exact := strings.Repeat("y", restGetMaxResponseBytes)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(exact))
	}))
	defer server.Close()
	source := "def on_group_text(message):\n    body, _ = rest_get(" + starQuote(server.URL) + ")\n    reply(message[\"channel\"], str(len(body)))\n"
	_, sender, _ := runHTTPCase(t, "at-cap", source, 20000, 500*time.Millisecond)
	got := sender.snapshot()
	if len(got) != 1 || got[0] != "1048576" {
		t.Fatalf("replies: got %v, want [1048576]", got)
	}
}

func TestRestGetBodyOverCapIsError(t *testing.T) {
	over := strings.Repeat("x", restGetMaxResponseBytes+1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(over))
	}))
	defer server.Close()
	source := "def on_group_text(message):\n    body, _ = rest_get(" + starQuote(server.URL) + ")\n    reply(message[\"channel\"], str(len(body)))\n"
	_, sender, runtime := runHTTPCase(t, "over-cap", source, 20000, 500*time.Millisecond)
	if got := sender.snapshot(); len(got) != 0 {
		t.Fatalf("over-cap body was returned to the script: %v", got)
	}
	current, err := runtime.Get(context.Background(), "over-cap")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if current.RuntimeStatus != "READY" {
		t.Fatalf("status: got %q, want READY", current.RuntimeStatus)
	}
}

func TestRestGetFollowsRedirects(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("landed"))
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirect.Close()
	source := "def on_group_text(message):\n    body, status = rest_get(" + starQuote(redirect.URL) + ")\n    reply(message[\"channel\"], \"%d %s\" % (status, body))\n"
	_, sender, _ := runHTTPCase(t, "redirect", source, 5000, 200*time.Millisecond)
	got := sender.snapshot()
	if len(got) != 1 || got[0] != "200 landed" {
		t.Fatalf("replies: got %v, want [200 landed]", got)
	}
}

func TestRestGetHonoursInvocationDeadline(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)
	source := "def on_group_text(message):\n    rest_get(" + starQuote(server.URL) + ")\n"
	_, sender, runtime := runHTTPCase(t, "deadline", source, 150, 400*time.Millisecond)
	if got := sender.snapshot(); len(got) != 0 {
		t.Fatalf("replies past the deadline: %v", got)
	}
	// Overrunning timeout_ms is the script's own timeout, so the existing policy
	// freezes it; a fetch error alone never does.
	current, err := runtime.Get(context.Background(), "deadline")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if current.RuntimeStatus != "FROZEN" {
		t.Fatalf("status: got %q, want FROZEN", current.RuntimeStatus)
	}
}

func TestRestGetConnectionFailureDoesNotFreeze(t *testing.T) {
	// A closed port fails fast; the script must survive a plain network error.
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := server.URL
	server.Close()
	source := "def on_group_text(message):\n    rest_get(" + starQuote(deadURL) + ")\n    reply(message[\"channel\"], \"reached\")\n"
	_, sender, runtime := runHTTPCase(t, "refused", source, 2000, 300*time.Millisecond)
	if got := sender.snapshot(); len(got) != 0 {
		t.Fatalf("replies after a refused connection: %v", got)
	}
	current, err := runtime.Get(context.Background(), "refused")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if current.RuntimeStatus != "READY" {
		t.Fatalf("status: got %q, want READY", current.RuntimeStatus)
	}
}

// TestRestGetGuardErrors pins the exact messages a script author sees.
func TestRestGetGuardErrors(t *testing.T) {
	cases := []struct {
		name    string
		args    starlark.Tuple
		wantMsg string
	}{
		{"scheme", starlark.Tuple{starlark.String("ftp://example.com/x")}, "rest_get: only http and https URLs are supported"},
		{"no scheme", starlark.Tuple{starlark.String("example.com/x")}, "rest_get: only http and https URLs are supported"},
		{"credentials", starlark.Tuple{starlark.String("http://user:pass@example.com/x")}, "rest_get: credentials in the URL are not supported"},
		{"headers not a dict", starlark.Tuple{starlark.String("http://example.com"), starlark.String("nope")}, "rest_get: headers must map strings to strings"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := hostRestGet(context.Background(), &Runtime{}, test.args, nil)
			if err == nil || err.Error() != test.wantMsg {
				t.Fatalf("error: got %v, want %q", err, test.wantMsg)
			}
		})
	}
}

func TestRestGetHeaderValueTypeError(t *testing.T) {
	dict := starlark.NewDict(1)
	if err := dict.SetKey(starlark.String("X-Key"), starlark.MakeInt(1)); err != nil {
		t.Fatal(err)
	}
	kwargs := []starlark.Tuple{{starlark.String("headers"), dict}}
	_, err := hostRestGet(context.Background(), &Runtime{}, starlark.Tuple{starlark.String("http://example.com")}, kwargs)
	if err == nil || err.Error() != "rest_get: headers must map strings to strings" {
		t.Fatalf("error: got %v", err)
	}
}
