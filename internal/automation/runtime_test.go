package automation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.starlark.net/starlark"
	"kam.bytecoder.nl/internal/db"
	"kam.bytecoder.nl/internal/kiss/parsers"
)

type testSender struct{ calls chan [2]string }

func (s *testSender) Send(ctx context.Context, channel, body string) error {
	select {
	case s.calls <- [2]string{channel, body}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type waitingSender struct{ entered chan struct{} }

func (s *waitingSender) Send(ctx context.Context, _, _ string) error {
	close(s.entered)
	<-ctx.Done()
	return ctx.Err()
}

func openTestRepository(t *testing.T) *db.AutomationRepository {
	t.Helper()
	repository, err := db.OpenAutomationRepository(filepath.Join(t.TempDir(), "automation.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	return repository
}

func TestRuntimeFreezesTimedOutScript(t *testing.T) {
	repository := openTestRepository(t)
	_, err := repository.Save(context.Background(), db.AutomationScript{
		ID: "loop", Name: "loop", Source: "def on_group_text(message):\n    for index in range(1000000000):\n        pass\n", Enabled: true, TimeoutMS: 20, RuntimeStatus: "READY",
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := NewRuntime(context.Background(), repository, &testSender{calls: make(chan [2]string, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()
	runtime.Handle(parsers.GroupTextMessage{ChannelName: "public", Body: "trigger"})
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		definition, err := repository.Get(context.Background(), "loop")
		if err == nil && definition.RuntimeStatus == "FROZEN" {
			if definition.LastError == "" {
				t.Fatal("frozen script did not persist last_error")
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for script freeze")
}

type recordingSender struct {
	mu    sync.Mutex
	calls []string
}

func (s *recordingSender) Send(_ context.Context, _, body string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, body)
	return nil
}

func (s *recordingSender) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// statusFailingRepository simulates SQLite refusing status writes.
type statusFailingRepository struct{ *db.AutomationRepository }

func (statusFailingRepository) SetStatus(context.Context, string, string, string) error {
	return errors.New("disk full")
}

func TestRuntimeStaysFrozenWhenPersistenceFails(t *testing.T) {
	repository := openTestRepository(t)
	save := func(id, source string, timeoutMS int64) {
		t.Helper()
		if _, err := repository.Save(context.Background(), db.AutomationScript{ID: id, Name: id, Source: source, Enabled: true, TimeoutMS: timeoutMS, RuntimeStatus: "READY"}, 0); err != nil {
			t.Fatal(err)
		}
	}
	save("a-loop", "def on_group_text(message):\n    reply(message[\"channel\"], \"loop\")\n    for index in range(1000000000):\n        pass\n", 20)
	save("z-probe", "def on_group_text(message):\n    reply(message[\"channel\"], \"probe\")\n", 500)
	sender := &recordingSender{}
	runtime, err := NewRuntime(context.Background(), statusFailingRepository{repository}, sender)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()
	runtime.Handle(parsers.GroupTextMessage{ChannelName: "public", Body: "first"})
	runtime.Handle(parsers.GroupTextMessage{ChannelName: "public", Body: "second"})
	deadline := time.Now().Add(2 * time.Second)
	for len(sender.snapshot()) < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	// The looping script ran for the first event only; it must not run again after its timeout.
	want := []string{"loop", "probe", "probe"}
	if got := sender.snapshot(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("replies: got %v, want %v", got, want)
	}
}

func TestRuntimeMarksUncompilableScriptBrokenAndKeepsOthersRunning(t *testing.T) {
	repository := openTestRepository(t)
	for id, source := range map[string]string{
		"a-broken": "def on_group_text(message:\n",
		"b-ok":     "def on_group_text(message):\n    reply(message[\"channel\"], \"ok\")\n",
	} {
		if _, err := repository.Save(context.Background(), db.AutomationScript{ID: id, Name: id, Source: source, Enabled: true, TimeoutMS: 500, RuntimeStatus: "READY"}, 0); err != nil {
			t.Fatal(err)
		}
	}
	sender := &testSender{calls: make(chan [2]string, 1)}
	runtime, err := NewRuntime(context.Background(), repository, sender)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()
	broken, err := repository.Get(context.Background(), "a-broken")
	if err != nil || broken.RuntimeStatus != "BROKEN" || broken.LastError == "" {
		t.Fatalf("uncompilable script not marked BROKEN: %+v, error %v", broken, err)
	}
	runtime.Handle(parsers.GroupTextMessage{ChannelName: "public", Body: "hello"})
	select {
	case call := <-sender.calls:
		if call[1] != "ok" {
			t.Fatalf("unexpected reply: %#v", call)
		}
	case <-time.After(time.Second):
		t.Fatal("healthy script did not run next to a broken one")
	}
}

func TestRuntimeStopCancelsInvocationAndDropsQueuedEvents(t *testing.T) {
	repository := openTestRepository(t)
	_, err := repository.Save(context.Background(), db.AutomationScript{
		ID: "sender", Name: "sender", Source: "def on_group_text(message):\n    reply(message[\"channel\"], \"reply\")\n", Enabled: true, TimeoutMS: 10_000, RuntimeStatus: "READY",
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	sender := &waitingSender{entered: make(chan struct{})}
	runtime, err := NewRuntimeWithQueueSize(context.Background(), repository, sender, 1)
	if err != nil {
		t.Fatal(err)
	}
	runtime.Handle(parsers.GroupTextMessage{ChannelName: "public", Body: "active"})
	select {
	case <-sender.entered:
	case <-time.After(time.Second):
		t.Fatal("script did not enter reply host function")
	}
	runtime.Handle(parsers.GroupTextMessage{ChannelName: "public", Body: "queued"})
	runtime.Handle(parsers.GroupTextMessage{ChannelName: "public", Body: "dropped"})
	runtime.Stop()
	if got := len(runtime.queue); got != 0 {
		t.Fatalf("queued events remain after stop: %d", got)
	}
	runtime.Handle(parsers.GroupTextMessage{ChannelName: "public", Body: "after stop"})
	if got := len(runtime.queue); got != 0 {
		t.Fatalf("event accepted after stop: queue length %d", got)
	}
}

func TestAPIRequiresRevisionForMutation(t *testing.T) {
	repository := openTestRepository(t)
	runtime, err := NewRuntime(context.Background(), repository, &testSender{calls: make(chan [2]string, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()
	handler := NewAPI(runtime)
	body := `{"script":{"id":"test","name":"test","source":"def on_group_text(message):\n    pass\n","enabled":true,"timeout_ms":500}}`
	response := serveAPI(handler, http.MethodPost, "/scripts", body)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status: got %d, body %s", response.Code, response.Body.String())
	}
	var script db.AutomationScript
	if err := json.Unmarshal(response.Body.Bytes(), &script); err != nil {
		t.Fatal(err)
	}
	if script.Revision != 1 {
		t.Fatalf("initial revision: got %d, want 1", script.Revision)
	}
	update := `{"expected_revision":1,"script":{"name":"test","source":"not valid Starlark !!!","enabled":true,"timeout_ms":500}}`
	response = serveAPI(handler, http.MethodPut, "/scripts/test", update)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid update status: got %d, want %d", response.Code, http.StatusUnprocessableEntity)
	}
	unchanged, err := repository.Get(context.Background(), "test")
	if err != nil || unchanged.Revision != 1 || unchanged.Source != script.Source {
		t.Fatalf("invalid update changed persisted script: %+v, error %v", unchanged, err)
	}
	response = serveAPI(handler, http.MethodPost, "/scripts/test/enable", `{"expected_revision":9}`)
	if response.Code != http.StatusConflict {
		t.Fatalf("stale revision status: got %d, want %d; body %s", response.Code, http.StatusConflict, response.Body.String())
	}
}

func TestAPIRecoveryClearsPersistedError(t *testing.T) {
	repository := openTestRepository(t)
	script, err := repository.Save(context.Background(), db.AutomationScript{
		ID: "recoverable", Name: "recoverable", Source: "def on_group_text(message):\n    pass\n", Enabled: true, TimeoutMS: 500, RuntimeStatus: "READY",
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.SetStatus(context.Background(), script.ID, "FROZEN", "timed out"); err != nil {
		t.Fatal(err)
	}
	runtime, err := NewRuntime(context.Background(), repository, &testSender{calls: make(chan [2]string, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()
	response := serveAPI(NewAPI(runtime), http.MethodPost, "/scripts/recoverable/recover", `{"expected_revision":1}`)
	if response.Code != http.StatusOK {
		t.Fatalf("recover status: got %d, body %s", response.Code, response.Body.String())
	}
	recovered, err := repository.Get(context.Background(), script.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.RuntimeStatus != "READY" || recovered.LastError != "" || recovered.Revision != 2 {
		t.Fatalf("unexpected recovered state: %+v", recovered)
	}
}

func TestAPIEnableAndDisableKeepFrozenStatus(t *testing.T) {
	repository := openTestRepository(t)
	script, err := repository.Save(context.Background(), db.AutomationScript{
		ID: "frozen", Name: "frozen", Source: "def on_group_text(message):\n    pass\n", Enabled: true, TimeoutMS: 500, RuntimeStatus: "READY",
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.SetStatus(context.Background(), script.ID, "FROZEN", "timed out"); err != nil {
		t.Fatal(err)
	}
	runtime, err := NewRuntime(context.Background(), repository, &testSender{calls: make(chan [2]string, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()
	handler := NewAPI(runtime)
	for revision, action := range []string{"disable", "enable"} {
		body := `{"expected_revision":` + strconv.Itoa(revision+1) + `}`
		if response := serveAPI(handler, http.MethodPost, "/scripts/frozen/"+action, body); response.Code != http.StatusOK {
			t.Fatalf("%s status: got %d, body %s", action, response.Code, response.Body.String())
		}
		current, err := repository.Get(context.Background(), script.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.RuntimeStatus != "FROZEN" || current.LastError != "timed out" {
			t.Fatalf("%s must not recover a frozen script: %+v", action, current)
		}
	}
}

func TestAPICreateDefaultsTimeoutAndListsEmptyArray(t *testing.T) {
	repository := openTestRepository(t)
	runtime, err := NewRuntime(context.Background(), repository, &testSender{calls: make(chan [2]string, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()
	handler := NewAPI(runtime)
	if response := serveAPI(handler, http.MethodGet, "/scripts", ""); strings.TrimSpace(response.Body.String()) != "[]" {
		t.Fatalf("empty list body: got %q, want []", response.Body.String())
	}
	body := `{"script":{"id":"defaults","name":"defaults","source":"def on_group_text(message):\n    pass\n"}}`
	response := serveAPI(handler, http.MethodPost, "/scripts", body)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status: got %d, body %s", response.Code, response.Body.String())
	}
	created, err := repository.Get(context.Background(), "defaults")
	if err != nil || created.TimeoutMS != 500 {
		t.Fatalf("default timeout: got %+v, error %v", created, err)
	}
}

func TestAPIReportsNameConflictAndMissingScripts(t *testing.T) {
	repository := openTestRepository(t)
	runtime, err := NewRuntime(context.Background(), repository, &testSender{calls: make(chan [2]string, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()
	handler := NewAPI(runtime)
	create := func(id, name string) int {
		body := `{"script":{"id":"` + id + `","name":"` + name + `","source":"def on_group_text(message):\n    pass\n"}}`
		return serveAPI(handler, http.MethodPost, "/scripts", body).Code
	}
	if code := create("one", "shared"); code != http.StatusCreated {
		t.Fatalf("first create: got %d", code)
	}
	if code := create("two", "shared"); code != http.StatusConflict {
		t.Fatalf("duplicate name: got %d, want %d", code, http.StatusConflict)
	}
	if code := create("one", "other"); code != http.StatusConflict {
		t.Fatalf("duplicate id: got %d, want %d", code, http.StatusConflict)
	}
	if code := serveAPI(handler, http.MethodDelete, "/scripts/ghost?revision=1", "").Code; code != http.StatusNotFound {
		t.Fatalf("delete missing: got %d, want %d", code, http.StatusNotFound)
	}
	if code := serveAPI(handler, http.MethodDelete, "/scripts/one?revision=7", "").Code; code != http.StatusConflict {
		t.Fatalf("delete stale revision: got %d, want %d", code, http.StatusConflict)
	}
	if code := serveAPI(handler, http.MethodPut, "/scripts/ghost", `{"expected_revision":1,"script":{"name":"ghost","source":"def on_group_text(message):\n    pass\n","timeout_ms":500}}`).Code; code != http.StatusNotFound {
		t.Fatalf("update missing: got %d, want %d", code, http.StatusNotFound)
	}
}

func TestAPIRejectsBrowserDrivenRequests(t *testing.T) {
	repository := openTestRepository(t)
	runtime, err := NewRuntime(context.Background(), repository, &testSender{calls: make(chan [2]string, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Stop()
	handler := NewAPI(runtime)
	body := `{"script":{"id":"csrf","name":"csrf","source":"def on_group_text(message):\n    pass\n"}}`

	rebound := httptest.NewRequest(http.MethodGet, "/scripts", nil)
	rebound.Host = "attacker.example:8765"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, rebound)
	if response.Code != http.StatusForbidden {
		t.Fatalf("foreign Host: got %d, want %d", response.Code, http.StatusForbidden)
	}

	form := httptest.NewRequest(http.MethodPost, "/scripts", strings.NewReader(body))
	form.Host = "127.0.0.1:8765"
	form.Header.Set("Content-Type", "text/plain")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, form)
	if response.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain body: got %d, want %d", response.Code, http.StatusUnsupportedMediaType)
	}
	if _, err := repository.Get(context.Background(), "csrf"); err == nil {
		t.Fatal("rejected request created a script")
	}
}

type funcSender func(context.Context, string, string) error

func (f funcSender) Send(ctx context.Context, channel, body string) error {
	return f(ctx, channel, body)
}

// gatedSender blocks every Send until release is closed or the send context ends.
type gatedSender struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGatedSender() *gatedSender {
	return &gatedSender{entered: make(chan struct{}), release: make(chan struct{})}
}

func (s *gatedSender) Send(ctx context.Context, _, _ string) error {
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.release:
	case <-ctx.Done():
	}
	return nil
}

// saveFailingRepository simulates SQLite refusing script writes.
type saveFailingRepository struct{ *db.AutomationRepository }

func (saveFailingRepository) Save(context.Context, db.AutomationScript, int64) (db.AutomationScript, error) {
	return db.AutomationScript{}, errors.New("disk full")
}

func replySource(text string) string {
	return fmt.Sprintf("def on_group_text(message):\n    reply(message[\"channel\"], %q)\n", text)
}

func saveScript(t *testing.T, repository *db.AutomationRepository, id, source string) db.AutomationScript {
	t.Helper()
	saved, err := repository.Save(context.Background(), db.AutomationScript{ID: id, Name: id, Source: source, Enabled: true, TimeoutMS: 500, RuntimeStatus: "READY"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

func newTestRuntime(t *testing.T, repository Repository, sender Sender) *Runtime {
	t.Helper()
	runtime, err := NewRuntime(context.Background(), repository, sender)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.Stop)
	return runtime
}

func waitForReplies(t *testing.T, sender *recordingSender, count int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(sender.snapshot()) < count && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	got := sender.snapshot()
	if len(got) < count {
		t.Fatalf("timed out waiting for %d replies; got %v", count, got)
	}
	return got
}

func TestValidateRejectsInvalidAndForbiddenSources(t *testing.T) {
	runtime := newTestRuntime(t, openTestRepository(t), &recordingSender{})
	cases := []struct {
		name    string
		source  string
		timeout int64
		valid   bool
	}{
		{"valid handler", "def on_group_text(message):\n    pass\n", 500, true},
		{"syntax error", "def on_group_text(message:\n", 500, false},
		{"missing handler", "VALUE = 1\n", 500, false},
		{"handler is not callable", "on_group_text = 1\n", 500, false},
		{"filesystem access", "def on_group_text(message):\n    open(\"/etc/passwd\")\n", 500, false},
		{"unknown module", "def on_group_text(message):\n    os.system(\"id\")\n", 500, false},
		{"load statement", "load(\"other.star\", \"helper\")\ndef on_group_text(message):\n    pass\n", 500, false},
		{"init exceeds timeout", "VALUE = len([i for i in range(1000000000)])\ndef on_group_text(message):\n    pass\n", 20, false},
		{"non-positive timeout", "def on_group_text(message):\n    pass\n", 0, false},
	}
	for _, test := range cases {
		err := runtime.Validate(context.Background(), "validate", test.source, test.timeout)
		if test.valid && err != nil {
			t.Errorf("%s: unexpected error %v", test.name, err)
		}
		if !test.valid && err == nil {
			t.Errorf("%s: invalid source accepted", test.name)
		}
	}
}

func TestHostFailuresDoNotFreezeScriptsOrBlockOthers(t *testing.T) {
	repository := openTestRepository(t)
	saveScript(t, repository, "a-failing-send", replySource("boom"))
	saveScript(t, repository, "b-bad-arguments", "def on_group_text(message):\n    reply(1)\n")
	saveScript(t, repository, "c-raises", "def on_group_text(message):\n    fail(\"script bug\")\n")
	saveScript(t, repository, "z-healthy", replySource("fine"))
	var mu sync.Mutex
	var bodies []string
	sender := funcSender(func(_ context.Context, _, body string) error {
		mu.Lock()
		defer mu.Unlock()
		bodies = append(bodies, body)
		if body == "boom" {
			return errors.New("radio busy")
		}
		return nil
	})
	runtime := newTestRuntime(t, repository, sender)
	runtime.Handle(parsers.GroupTextMessage{ChannelName: "public", Body: "hello"})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		count := len(bodies)
		mu.Unlock()
		if count >= 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	got := append([]string(nil), bodies...)
	mu.Unlock()
	if strings.Join(got, ",") != "boom,fine" {
		t.Fatalf("sends: got %v, want [boom fine]", got)
	}
	for _, id := range []string{"a-failing-send", "b-bad-arguments", "c-raises", "z-healthy"} {
		script, err := repository.Get(context.Background(), id)
		if err != nil || script.RuntimeStatus != "READY" || script.LastError != "" {
			t.Errorf("%s: host or script errors must not change status: %+v, error %v", id, script, err)
		}
	}
}

func TestReplyTimeoutIsAHostErrorNotAFreeze(t *testing.T) {
	repository := openTestRepository(t)
	// The script budget must outlast the 500 ms reply budget, or the timeout would be the script's.
	if _, err := repository.Save(context.Background(), db.AutomationScript{ID: "slow-send", Name: "slow-send", Source: replySource("never delivered"), Enabled: true, TimeoutMS: 10_000, RuntimeStatus: "READY"}, 0); err != nil {
		t.Fatal(err)
	}
	returned := make(chan error, 1)
	runtime := newTestRuntime(t, repository, funcSender(func(ctx context.Context, _, _ string) error {
		<-ctx.Done()
		returned <- ctx.Err()
		return ctx.Err()
	}))
	runtime.Handle(parsers.GroupTextMessage{ChannelName: "public", Body: "hello"})
	select {
	case err := <-returned:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("send context: got %v, want deadline exceeded", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reply was not bounded by its own timeout")
	}
	script, err := repository.Get(context.Background(), "slow-send")
	if err != nil || script.RuntimeStatus != "READY" {
		t.Fatalf("reply timeout must not freeze the script: %+v, error %v", script, err)
	}
}

func TestEventsAndScriptsRunInOrder(t *testing.T) {
	repository := openTestRepository(t)
	for _, id := range []string{"b", "a"} {
		saveScript(t, repository, id, fmt.Sprintf("def on_group_text(message):\n    reply(message[\"channel\"], %q + message[\"body\"])\n", id+":"))
	}
	sender := &recordingSender{}
	runtime := newTestRuntime(t, repository, sender)
	for _, body := range []string{"1", "2", "3"} {
		runtime.Handle(parsers.GroupTextMessage{ChannelName: "public", Body: body})
	}
	got := waitForReplies(t, sender, 6)
	if want := "a:1,b:1,a:2,b:2,a:3,b:3"; strings.Join(got, ",") != want {
		t.Fatalf("order: got %v, want %s", got, want)
	}
}

func TestHandleNeverBlocksWhenQueueIsFull(t *testing.T) {
	repository := openTestRepository(t)
	saveScript(t, repository, "sender", replySource("reply"))
	sender := newGatedSender()
	runtime, err := NewRuntimeWithQueueSize(context.Background(), repository, sender, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		close(sender.release)
		runtime.Stop()
	})
	runtime.Handle(parsers.GroupTextMessage{ChannelName: "public", Body: "occupies the worker"})
	select {
	case <-sender.entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not start the first event")
	}
	started := time.Now()
	for range 100 {
		runtime.Handle(parsers.GroupTextMessage{ChannelName: "public", Body: "flood"})
	}
	if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
		t.Fatalf("Handle blocked on a full queue: %v", elapsed)
	}
	if got := len(runtime.queue); got != 1 {
		t.Fatalf("queue length: got %d, want 1 (the rest must be dropped)", got)
	}
}

func TestSaveActivatesNewSourceForNextEvent(t *testing.T) {
	repository := openTestRepository(t)
	script := saveScript(t, repository, "swap", replySource("old"))
	sender := &recordingSender{}
	runtime := newTestRuntime(t, repository, sender)
	runtime.Handle(parsers.GroupTextMessage{ChannelName: "public", Body: "1"})
	waitForReplies(t, sender, 1)
	if _, err := runtime.Save(context.Background(), db.AutomationScript{ID: "swap", Name: "swap", Source: replySource("new"), Enabled: true, TimeoutMS: 500}, script.Revision); err != nil {
		t.Fatal(err)
	}
	runtime.Handle(parsers.GroupTextMessage{ChannelName: "public", Body: "2"})
	if got := waitForReplies(t, sender, 2); strings.Join(got, ",") != "old,new" {
		t.Fatalf("replies: got %v, want [old new]", got)
	}
}

func TestFailedPersistenceKeepsPreviousProgramActive(t *testing.T) {
	repository := openTestRepository(t)
	script := saveScript(t, repository, "keep", replySource("old"))
	sender := &recordingSender{}
	runtime := newTestRuntime(t, saveFailingRepository{repository}, sender)
	if _, err := runtime.Save(context.Background(), db.AutomationScript{ID: "keep", Name: "keep", Source: replySource("new"), Enabled: true, TimeoutMS: 500}, script.Revision); err == nil {
		t.Fatal("Save succeeded although persistence failed")
	}
	runtime.Handle(parsers.GroupTextMessage{ChannelName: "public", Body: "1"})
	if got := waitForReplies(t, sender, 1); got[0] != "old" {
		t.Fatalf("unpersisted source became active: %v", got)
	}
	response := serveAPI(NewAPI(runtime), http.MethodPost, "/scripts", `{"script":{"id":"fresh","name":"fresh","source":"def on_group_text(message):\n    pass\n"}}`)
	if response.Code < 400 {
		t.Fatalf("persistence failure reported as success: %d", response.Code)
	}
}

func TestBrokenScriptRecoversOnlyWithValidSource(t *testing.T) {
	repository := openTestRepository(t)
	script, err := repository.Save(context.Background(), db.AutomationScript{ID: "fix", Name: "fix", Source: "def on_group_text(message:\n", Enabled: true, TimeoutMS: 500, RuntimeStatus: "BROKEN", LastError: "syntax error"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	sender := &recordingSender{}
	runtime := newTestRuntime(t, repository, sender)
	handler := NewAPI(runtime)

	if response := serveAPI(handler, http.MethodPost, "/scripts/fix/recover", `{"expected_revision":1}`); response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("recover with invalid source: got %d, want %d", response.Code, http.StatusUnprocessableEntity)
	}
	if still, _ := repository.Get(context.Background(), "fix"); still.RuntimeStatus != "BROKEN" || still.LastError != "syntax error" || still.Revision != script.Revision {
		t.Fatalf("failed recovery changed the script: %+v", still)
	}

	update, err := json.Marshal(map[string]any{"expected_revision": script.Revision, "script": map[string]any{"name": "fix", "source": replySource("fixed"), "enabled": true, "timeout_ms": 500}})
	if err != nil {
		t.Fatal(err)
	}
	if response := serveAPI(handler, http.MethodPut, "/scripts/fix", string(update)); response.Code != http.StatusOK {
		t.Fatalf("fixing source: got %d, body %s", response.Code, response.Body.String())
	}
	if fixed, _ := repository.Get(context.Background(), "fix"); fixed.RuntimeStatus != "READY" || fixed.LastError != "" {
		t.Fatalf("source update must return the script to READY: %+v", fixed)
	}
	runtime.Handle(parsers.GroupTextMessage{ChannelName: "public", Body: "1"})
	if got := waitForReplies(t, sender, 1); got[0] != "fixed" {
		t.Fatalf("recovered script replies: %v", got)
	}
}

func TestLiveUpdatesWhileEventsArrive(t *testing.T) {
	repository := openTestRepository(t)
	script := saveScript(t, repository, "live", replySource("v0"))
	sender := &recordingSender{}
	runtime := newTestRuntime(t, repository, sender)
	stop := make(chan struct{})
	var producer sync.WaitGroup
	producer.Add(1)
	go func() {
		defer producer.Done()
		for {
			select {
			case <-stop:
				return
			default:
				runtime.Handle(parsers.GroupTextMessage{ChannelName: "public", Body: "tick"})
				time.Sleep(100 * time.Microsecond)
			}
		}
	}()
	revision := script.Revision
	const versions = 20
	for version := 1; version <= versions; version++ {
		saved, err := runtime.Save(context.Background(), db.AutomationScript{ID: "live", Name: "live", Source: replySource("v" + strconv.Itoa(version)), Enabled: true, TimeoutMS: 500}, revision)
		if err != nil {
			t.Fatal(err)
		}
		revision = saved.Revision
	}
	close(stop)
	producer.Wait()
	for len(runtime.queue) > 0 {
		time.Sleep(time.Millisecond)
	}
	runtime.Handle(parsers.GroupTextMessage{ChannelName: "public", Body: "final"})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := sender.snapshot(); len(got) > 0 && got[len(got)-1] == "v"+strconv.Itoa(versions) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	replies := sender.snapshot()
	if len(replies) == 0 || replies[len(replies)-1] != "v"+strconv.Itoa(versions) {
		t.Fatalf("latest source not active; replies %v", replies)
	}
	previous := 0
	for _, reply := range replies {
		version, err := strconv.Atoi(strings.TrimPrefix(reply, "v"))
		if err != nil || version < previous {
			t.Fatalf("an older program ran after a newer one was published: %v", replies)
		}
		previous = version
	}
}

func TestMessageConversionExposesDocumentedFieldsReadOnly(t *testing.T) {
	message := parsers.GroupTextMessage{
		ChannelName: "Public", Sender: "alice", Body: "hi @kam", Timestamp: time.Unix(1_700_000_000, 0),
		HopCount: 2, HopStations: [][]byte{{0xab, 0x01}}, HasRxMeta: true, SNR: -7, RSSI: -91, TimeDriftSeconds: 3,
	}
	dict, ok := toStarlarkMessage(message).(*starlark.Dict)
	if !ok {
		t.Fatal("message is not a dict")
	}
	want := map[string]string{
		"channel": `"#public"`, "sender": `"alice"`, "body": `"hi @kam"`, "timestamp_unix": "1700000000", "hop_count": "2",
		"hop_stations": `["ab01"]`, "has_rx_meta": "True", "snr_quarter_db": "-7", "rssi_dbm": "-91", "time_drift_seconds": "3",
	}
	if dict.Len() != len(want) {
		t.Fatalf("field count: got %d, want %d", dict.Len(), len(want))
	}
	for key, expected := range want {
		value, found, err := dict.Get(starlark.String(key))
		if err != nil || !found || value.String() != expected {
			t.Errorf("%s: got %v (found %v, error %v), want %s", key, value, found, err, expected)
		}
	}
	if err := dict.SetKey(starlark.String("channel"), starlark.String("#other")); err == nil {
		t.Error("message dict is mutable")
	}
	stations, _, _ := dict.Get(starlark.String("hop_stations"))
	if err := stations.(*starlark.List).Append(starlark.String("ff")); err == nil {
		t.Error("hop_stations list is mutable")
	}

	clone := cloneMessage(message)
	message.HopStations[0][0] = 0xff
	if clone.HopStations[0][0] != 0xab {
		t.Error("queued message shares hop station memory with the parser")
	}
}

func TestHandlePassesHopStationsToScripts(t *testing.T) {
	repository := openTestRepository(t)
	saveScript(t, repository, "path", "def on_group_text(message):\n    reply(message[\"channel\"], \",\".join(message[\"hop_stations\"]))\n")
	sender := &recordingSender{}
	runtime := newTestRuntime(t, repository, sender)
	runtime.Handle(parsers.GroupTextMessage{ChannelName: "public", Body: "hi", HopCount: 2, HopStations: [][]byte{{0xab, 0x01}, {0xcd}}})
	if got := waitForReplies(t, sender, 1); got[0] != "ab01,cd" {
		t.Fatalf("hop stations seen by the script: got %q, want %q", got[0], "ab01,cd")
	}
}

func TestAPIUpdatesUseOptimisticConcurrency(t *testing.T) {
	repository := openTestRepository(t)
	script := saveScript(t, repository, "cas", replySource("v1"))
	handler := NewAPI(newTestRuntime(t, repository, &recordingSender{}))
	update := func(revision int64, text string) int {
		body, _ := json.Marshal(map[string]any{"expected_revision": revision, "script": map[string]any{"name": "cas", "source": replySource(text), "enabled": true, "timeout_ms": 500}})
		return serveAPI(handler, http.MethodPut, "/scripts/cas", string(body)).Code
	}
	if code := update(script.Revision, "v2"); code != http.StatusOK {
		t.Fatalf("first update: got %d", code)
	}
	if code := update(script.Revision, "v3"); code != http.StatusConflict {
		t.Fatalf("update with stale revision: got %d, want %d", code, http.StatusConflict)
	}
	if current, _ := repository.Get(context.Background(), "cas"); current.Source != replySource("v2") || current.Revision != script.Revision+1 {
		t.Fatalf("stale update changed the script: %+v", current)
	}
}

func TestAPIServesOverRealTransportOnlyToLocalClients(t *testing.T) {
	runtime := newTestRuntime(t, openTestRepository(t), &recordingSender{})
	server := httptest.NewServer(NewAPI(runtime))
	defer server.Close()
	body := `{"script":{"id":"wire","name":"wire","source":"def on_group_text(message):\n    pass\n"}}`
	response, err := server.Client().Post(server.URL+"/scripts", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create over HTTP: got %d", response.StatusCode)
	}
	request, err := http.NewRequest(http.MethodGet, server.URL+"/scripts", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "attacker.example"
	response, err = server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("rebound Host over HTTP: got %d, want %d", response.StatusCode, http.StatusForbidden)
	}
}

func TestLocalAPIRefusesNonLoopbackBinding(t *testing.T) {
	listener, err := listenLoopback("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener.Close()
	for _, address := range []string{"0.0.0.0:0", "[::]:0"} {
		if listener, err := listenLoopback(address); err == nil {
			listener.Close()
			t.Errorf("%s: wildcard binding accepted", address)
		}
	}
	if !isLoopbackHost(LocalAPIAddress) {
		t.Errorf("default address %s is not loopback", LocalAPIAddress)
	}
	for host, want := range map[string]bool{
		"127.0.0.1:8765": true, "localhost:8765": true, "LOCALHOST": true, "[::1]:8765": true, "127.0.0.1": true,
		"attacker.example:8765": false, "192.168.1.5:8765": false, "0.0.0.0:8765": false, "127.0.0.1.attacker.example": false, "": false,
	} {
		if got := isLoopbackHost(host); got != want {
			t.Errorf("isLoopbackHost(%q): got %v, want %v", host, got, want)
		}
	}
}

// serveAPI issues a request as a local client would: loopback Host and a JSON body.
func TestAPIIndexListsRoutes(t *testing.T) {
	runtime := newTestRuntime(t, openTestRepository(t), &recordingSender{})
	response := serveAPI(NewAPI(runtime), http.MethodGet, "/", "")
	if response.Code != http.StatusOK {
		t.Fatalf("index status: got %d, body %s", response.Code, response.Body.String())
	}
	var index struct {
		API       string        `json:"api"`
		Endpoints []apiEndpoint `json:"endpoints"`
		Docs      string        `json:"docs"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &index); err != nil {
		t.Fatal(err)
	}
	if index.API != "kam" || index.Docs == "" || len(index.Endpoints) != 9 {
		t.Fatalf("index: %+v", index)
	}
	seen := map[string]bool{}
	for _, item := range index.Endpoints {
		if item.Purpose == "" {
			t.Fatalf("endpoint without purpose: %+v", item)
		}
		seen[item.Method+" "+item.Path] = true
	}
	for _, want := range []string{
		"GET /scripts", "POST /scripts", "GET /scripts/{id}", "PUT /scripts/{id}",
		"DELETE /scripts/{id}?revision=N", "POST /scripts/validate",
		"POST /scripts/{id}/enable", "POST /scripts/{id}/disable", "POST /scripts/{id}/recover",
	} {
		if !seen[want] {
			t.Fatalf("index missing %s: %+v", want, seen)
		}
	}
}

func serveAPI(handler http.Handler, method, target, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Host = "127.0.0.1:8765"
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
