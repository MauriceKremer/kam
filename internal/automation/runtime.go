package automation

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	starlarkjson "go.starlark.net/lib/json"
	starlarktime "go.starlark.net/lib/time"
	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
	"kam.bytecoder.nl/internal/db"
	"kam.bytecoder.nl/internal/kiss/parsers"
)

const defaultQueueSize = 128
const defaultReplyTimeout = 500 * time.Millisecond

// restGetMaxResponseBytes caps a rest_get body; a longer body is an error, not a
// silent truncation, because a cut JSON document still decodes to something.
const restGetMaxResponseBytes = 1 << 20

var errScriptTimeout = errors.New("script execution timed out")

// defaultTimeoutMS is the timeout_ms a script defines or validates without
// its own; the capability applies it, not the handler (D7).
const defaultTimeoutMS = int64(db.DefaultAutomationTimeout / time.Millisecond)

// Script domain sentinels, aliased from the storage layer so callers classify
// failures by what they mean and never name the storage engine (D8).
var (
	ErrScriptNotFound  = db.ErrAutomationNotFound
	ErrScriptConflict  = db.ErrAutomationConflict
	ErrScriptNameTaken = db.ErrAutomationNameTaken
)

type Sender interface {
	Send(context.Context, string, string) error
}

// RoomPoster publishes a post in a room. The room implementation is injected;
// internal/automation never imports internal/room.
type RoomPoster interface {
	Post(ctx context.Context, roomID, text string) error
}

type Repository interface {
	List(context.Context) ([]db.AutomationScript, error)
	Get(context.Context, string) (db.AutomationScript, error)
	Save(context.Context, db.AutomationScript, int64) (db.AutomationScript, error)
	Delete(context.Context, string, int64) error
	SetStatus(context.Context, string, string, string) error
}

type compiledScript struct {
	definition db.AutomationScript
	program    *starlark.Program
	interval   int64 // timer_interval_minutes as compiled, 0 = no timer
	message    bool  // on_group_text is defined and callable
	timer      bool  // on_timer is defined and callable
}

// hostFunction is a capability scripts may call; ctx carries the invocation deadline and must be honoured.
type hostFunction func(ctx context.Context, r *Runtime, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error)

// hostFunctions is the complete set of names visible to scripts; anything else fails compilation.
var hostFunctions = map[string]hostFunction{
	"reply":     hostReply,
	"rest_get":  hostRestGet,
	"room_post": hostRoomPost,
}

// predeclaredModules are the library modules every script sees without load().
var predeclaredModules = starlark.StringDict{
	"json": starlarkjson.Module,
	"time": starlarktime.Module,
}

// minTimerInterval is the smallest timer period a script may declare; tests lower it.
var minTimerInterval = time.Minute

// timerUnit is one declared minute of interval in real time. Tests shorten it
// so a timer can be observed without waiting a minute.
var timerUnit = time.Minute

// isPredeclared reports whether a name is visible without being defined by the script.
func isPredeclared(name string) bool {
	if _, ok := hostFunctions[name]; ok {
		return true
	}
	_, ok := predeclaredModules[name]
	return ok
}

func hostReply(ctx context.Context, r *Runtime, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var channel, text string
	if err := starlark.UnpackArgs("reply", args, kwargs, "channel", &channel, "text", &text); err != nil {
		return starlark.None, err
	}
	sendContext, cancel := context.WithTimeout(ctx, defaultReplyTimeout)
	defer cancel()
	return starlark.None, r.sender.Send(sendContext, channel, text)
}

func hostRestGet(ctx context.Context, r *Runtime, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var rawURL string
	var rawHeaders starlark.Value = starlark.None
	if err := starlark.UnpackArgs("rest_get", args, kwargs, "url", &rawURL, "headers?", &rawHeaders); err != nil {
		return starlark.None, err
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return starlark.None, fmt.Errorf("rest_get: invalid URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return starlark.None, errors.New("rest_get: only http and https URLs are supported")
	}
	if parsed.User != nil {
		return starlark.None, errors.New("rest_get: credentials in the URL are not supported")
	}
	headers := http.Header{}
	if rawHeaders != starlark.None {
		dict, ok := rawHeaders.(*starlark.Dict)
		if !ok {
			return starlark.None, errors.New("rest_get: headers must map strings to strings")
		}
		for _, item := range dict.Items() {
			name, nameOK := starlark.AsString(item[0])
			value, valueOK := starlark.AsString(item[1])
			if !nameOK || !valueOK {
				return starlark.None, errors.New("rest_get: headers must map strings to strings")
			}
			headers.Add(name, value)
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return starlark.None, fmt.Errorf("rest_get: invalid URL: %w", err)
	}
	request.Header = headers
	response, err := r.httpClient.Do(request)
	if err != nil {
		return starlark.None, fmt.Errorf("rest_get: %w", err)
	}
	defer response.Body.Close()
	// Read one byte past the cap so an over-long body is distinguishable from one
	// that fits exactly.
	body, err := io.ReadAll(io.LimitReader(response.Body, restGetMaxResponseBytes+1))
	if err != nil {
		return starlark.None, fmt.Errorf("rest_get: %w", err)
	}
	if len(body) > restGetMaxResponseBytes {
		return starlark.None, fmt.Errorf("rest_get: response exceeds %s bytes", strconv.Itoa(restGetMaxResponseBytes))
	}
	return starlark.Tuple{starlark.String(body), starlark.MakeInt(response.StatusCode)}, nil
}

func hostRoomPost(ctx context.Context, r *Runtime, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var roomID, text string
	if err := starlark.UnpackArgs("room_post", args, kwargs, "room_id", &roomID, "text", &text); err != nil {
		return starlark.None, err
	}
	if roomID == "" {
		return starlark.None, errors.New("room_post: room id is required")
	}
	if text == "" {
		return starlark.None, errors.New("room_post: text is empty")
	}
	if r.poster == nil {
		return starlark.None, errors.New("room_post: rooms are not running")
	}
	return starlark.None, r.poster.Post(ctx, roomID, text)
}

// runtimeEvent is one unit of work for the worker: a group text or a timer
// fire. Exactly one field is set.
type runtimeEvent struct {
	message *parsers.GroupTextMessage
	timer   *timerFire
}

// timerFire asks the worker to run one script's on_timer.
type timerFire struct {
	id string
}

type Runtime struct {
	repository Repository
	sender     Sender
	poster     RoomPoster
	queue      chan runtimeEvent
	httpClient *http.Client
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
	timerDone  chan struct{}
	// timerChanged nudges the timer loop to re-read the schedule. It carries no
	// payload: the loop always recomputes from the scripts map.
	timerChanged chan struct{}

	mu             sync.RWMutex
	mutationMu     sync.Mutex
	intakeMu       sync.Mutex
	scripts        map[string]*compiledScript
	once           sync.Once
	monitorDropped atomic.Uint64
}

func (r *Runtime) MonitorStats() (enabled int, depth int, dropped uint64) {
	r.mu.RLock()
	for _, script := range r.scripts {
		if script.definition.Enabled {
			enabled++
		}
	}
	r.mu.RUnlock()
	return enabled, len(r.queue), r.monitorDropped.Load()
}

// NewRuntime builds a runtime; a nil poster makes room_post fail at invoke
// time, which is what a node without rooms wants.
func NewRuntime(parent context.Context, repository Repository, sender Sender, poster RoomPoster) (*Runtime, error) {
	return NewRuntimeWithQueueSize(parent, repository, sender, poster, defaultQueueSize)
}

func NewRuntimeWithQueueSize(parent context.Context, repository Repository, sender Sender, poster RoomPoster, queueSize int) (*Runtime, error) {
	if queueSize <= 0 {
		return nil, errors.New("automation queue size must be positive")
	}
	definitions, err := repository.List(parent)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	runtime := &Runtime{
		repository:   repository,
		sender:       sender,
		poster:       poster,
		queue:        make(chan runtimeEvent, queueSize),
		httpClient:   &http.Client{Transport: &http.Transport{TLSHandshakeTimeout: 10 * time.Second}},
		ctx:          ctx,
		cancel:       cancel,
		done:         make(chan struct{}),
		timerDone:    make(chan struct{}),
		timerChanged: make(chan struct{}, 1),
		scripts:      make(map[string]*compiledScript, len(definitions)),
	}
	for _, definition := range definitions {
		program, compiled, err := compile(definition.ID, definition.Source, definition.TimeoutMS)
		if err != nil {
			if definition.RuntimeStatus == "READY" {
				definition.RuntimeStatus = "BROKEN"
				definition.LastError = err.Error()
				if persistErr := repository.SetStatus(parent, definition.ID, "BROKEN", err.Error()); persistErr != nil {
					slog.Error("[Automation] failed to persist broken script status", "script_id", definition.ID, "error", persistErr)
				}
			}
			slog.Error("[Automation] script is not runnable", "script_id", definition.ID, "error", err)
		}
		if err == nil {
			runtime.scripts[definition.ID] = &compiledScript{definition: definition, program: program, interval: compiled.interval, message: compiled.messageEntry, timer: compiled.timerEntry}
		}
	}
	go runtime.worker()
	go runtime.timerLoop()
	return runtime, nil
}

func (r *Runtime) Handle(message parsers.GroupTextMessage) {
	copy := cloneMessage(message)
	r.intakeMu.Lock()
	defer r.intakeMu.Unlock()
	if r.ctx.Err() != nil {
		return
	}
	select {
	case r.queue <- runtimeEvent{message: &copy}:
	default:
		r.monitorDropped.Add(1)
		slog.Warn("[Automation] group-text event dropped: queue full", "channel", message.ChannelName, "sender", message.Sender)
	}
}

func (r *Runtime) worker() {
	defer close(r.done)
	for {
		select {
		case <-r.ctx.Done():
			r.dropQueuedEvents(0)
			return
		case event := <-r.queue:
			if r.ctx.Err() != nil {
				r.dropQueuedEvents(1)
				return
			}
			switch {
			case event.message != nil:
				r.dispatch(*event.message)
			case event.timer != nil:
				r.dispatchTimer(*event.timer)
			}
		}
	}
}

func (r *Runtime) dropQueuedEvents(alreadyDropped int) {
	dropped, timers := alreadyDropped, 0
	for {
		select {
		case event := <-r.queue:
			if event.timer != nil {
				timers++
				continue
			}
			dropped++
		default:
			if dropped+timers != 0 {
				slog.Info("[Automation] queued events dropped during shutdown", "group_texts", dropped, "timers", timers)
			}
			return
		}
	}
}

// timerLoop schedules on_timer for every timer script. It keeps one next fire
// time per script id and recomputes the whole schedule whenever the scripts map
// may have changed, so there is no per-script bookkeeping to get wrong.
func (r *Runtime) timerLoop() {
	defer close(r.timerDone)
	next := make(map[string]time.Time)
	for {
		schedule := r.timerIntervals()
		now := time.Now()
		for id := range next {
			if _, ok := schedule[id]; !ok {
				delete(next, id)
			}
		}
		var wake time.Time
		for id, interval := range schedule {
			if _, ok := next[id]; !ok {
				next[id] = now.Add(interval)
			}
			if wake.IsZero() || next[id].Before(wake) {
				wake = next[id]
			}
		}
		if !wake.IsZero() {
			timer := time.NewTimer(time.Until(wake))
			select {
			case <-r.ctx.Done():
				timer.Stop()
				return
			case <-r.timerChanged:
				timer.Stop()
				continue
			case <-timer.C:
			}
		} else {
			select {
			case <-r.ctx.Done():
				return
			case <-r.timerChanged:
				continue
			}
		}
		// Fire every script whose time has come. The next fire is measured from
		// now, so a slow worker never produces a burst of catch-up fires.
		fired := time.Now()
		for _, id := range sortedIDs(next) {
			if next[id].After(fired) {
				continue
			}
			select {
			case r.queue <- runtimeEvent{timer: &timerFire{id: id}}:
			default:
				r.monitorDropped.Add(1)
				slog.Warn("[Automation] timer fire dropped: queue full", "script_id", id)
			}
			next[id] = fired.Add(schedule[id])
		}
	}
}

// timerIntervals snapshots the scripts a timer applies to: resident, enabled,
// ready, and declaring an interval. FROZEN and BROKEN scripts are idle.
func (r *Runtime) timerIntervals() map[string]time.Duration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	schedule := make(map[string]time.Duration)
	for id, script := range r.scripts {
		if script.definition.Enabled && script.definition.RuntimeStatus == "READY" && script.interval > 0 {
			schedule[id] = time.Duration(script.interval) * timerUnit
		}
	}
	return schedule
}

// TimerIntervals reports each timer script's period in minutes, for the API.
func (r *Runtime) TimerIntervals() map[string]int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	intervals := make(map[string]int64)
	for id, script := range r.scripts {
		if script.interval > 0 {
			intervals[id] = script.interval
		}
	}
	return intervals
}

// nudgeTimer tells the loop that the scripts map changed. It never blocks: a
// pending nudge already covers any number of changes.
func (r *Runtime) nudgeTimer() {
	select {
	case r.timerChanged <- struct{}{}:
	default:
	}
}

// dispatchTimer runs one timer script if it is still eligible: a fire can race
// a disable, a freeze, or a delete.
func (r *Runtime) dispatchTimer(fire timerFire) {
	r.mu.RLock()
	script, ok := r.scripts[fire.id]
	eligible := ok && script.definition.Enabled && script.definition.RuntimeStatus == "READY" && script.interval > 0
	r.mu.RUnlock()
	if !eligible || r.ctx.Err() != nil {
		return
	}
	if err := r.invokeTimer(script, time.Now()); err != nil {
		if errors.Is(err, errScriptTimeout) {
			r.freeze(script, err)
		} else if r.ctx.Err() == nil {
			slog.Warn("[Automation] timer invocation failed", "script_id", script.definition.ID, "error", err)
		}
	}
}

func (r *Runtime) invokeTimer(script *compiledScript, now time.Time) error {
	argument := starlark.NewDict(1)
	if err := argument.SetKey(starlark.String("timestamp_unix"), starlark.MakeInt64(now.Unix())); err != nil {
		return err
	}
	argument.Freeze()
	return r.invoke(script, "on_timer", starlark.Tuple{argument})
}

// sortedIDs returns the map keys in a stable order so equal intervals fire
// deterministically.
func sortedIDs(times map[string]time.Time) []string {
	ids := make([]string, 0, len(times))
	for id := range times {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (r *Runtime) dispatch(message parsers.GroupTextMessage) {
	r.mu.RLock()
	snapshot := make([]*compiledScript, 0, len(r.scripts))
	for _, script := range r.scripts {
		if script.definition.Enabled && script.definition.RuntimeStatus == "READY" && script.message {
			snapshot = append(snapshot, script)
		}
	}
	r.mu.RUnlock()
	sort.Slice(snapshot, func(i, j int) bool {
		return snapshot[i].definition.ID < snapshot[j].definition.ID
	})
	for _, script := range snapshot {
		if r.ctx.Err() != nil {
			return
		}
		if err := r.invoke(script, "on_group_text", starlark.Tuple{toStarlarkMessage(message)}); err != nil {
			if errors.Is(err, errScriptTimeout) {
				r.freeze(script, err)
			} else if r.ctx.Err() == nil {
				slog.Warn("[Automation] script invocation failed", "script_id", script.definition.ID, "error", err)
			}
		}
	}
}

func (r *Runtime) invoke(script *compiledScript, entry string, args starlark.Tuple) error {
	timeout := timeoutDuration(script.definition.TimeoutMS)
	ctx, cancel := context.WithTimeout(r.ctx, timeout)
	defer cancel()
	thread := &starlark.Thread{Name: script.definition.ID}
	stopInterrupt := context.AfterFunc(ctx, func() { thread.Cancel("script execution interrupted") })
	defer stopInterrupt()
	predeclared := predeclaredValues(func(name string, function hostFunction) starlark.Value {
		return starlark.NewBuiltin(name, func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			return function(ctx, r, args, kwargs)
		})
	})
	globals, err := script.program.Init(thread, predeclared)
	if err != nil {
		return classifyInvokeError(ctx, timeout, err)
	}
	// Dispatch only queues scripts that declare the entry, so a missing one means
	// the resident program was replaced between snapshot and invoke.
	handler, ok := globals[entry]
	if !ok {
		return fmt.Errorf("script does not define %s", entry)
	}
	if _, err := starlark.Call(thread, handler, args, nil); err != nil {
		return classifyInvokeError(ctx, timeout, err)
	}
	return nil
}

// classifyInvokeError maps an interpreter error to errScriptTimeout or the runtime's own cancellation.
func classifyInvokeError(ctx context.Context, timeout time.Duration, err error) error {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("%w after %s", errScriptTimeout, timeout)
	case ctx.Err() != nil:
		return ctx.Err()
	}
	return err
}

func (r *Runtime) freeze(script *compiledScript, cause error) {
	r.mutationMu.Lock()
	defer r.mutationMu.Unlock()
	id := script.definition.ID
	r.mu.Lock()
	current := r.scripts[id]
	// Compare programs, not pointers: enable/disable republish the same program.
	if current == nil || current.program != script.program || current.definition.RuntimeStatus != "READY" {
		r.mu.Unlock()
		return
	}
	frozen := *current
	frozen.definition.RuntimeStatus = "FROZEN"
	frozen.definition.LastError = cause.Error()
	r.scripts[id] = &frozen
	r.mu.Unlock()
	slog.Error("[Automation] script frozen after timeout", "script_id", id, "error", cause)
	// Shutdown must not abort recording a timeout that already happened.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), 5*time.Second)
	defer cancel()
	if err := r.repository.SetStatus(ctx, id, "FROZEN", cause.Error()); err != nil {
		slog.Error("[Automation] failed to persist frozen script status; it stays frozen until restart", "script_id", id, "error", err)
	}
}

func (r *Runtime) Stop() {
	r.once.Do(func() {
		r.intakeMu.Lock()
		r.cancel()
		r.intakeMu.Unlock()
	})
	<-r.done
	<-r.timerDone
	r.waitForInFlightMutation()
}

// waitForInFlightMutation waits out a mutation already holding mutationMu.
// Save, SetEnabled and Delete write to the repository under that lock, and
// Stop's caller closes the repository next, hence the empty critical section:
// it is a wait, not a guard. Whoever acquires the lock after it finds
// r.ctx cancelled and returns before writing, and freeze always runs on the
// worker, which has exited by <-r.done.
func (r *Runtime) waitForInFlightMutation() {
	r.mutationMu.Lock()
	r.mutationMu.Unlock()
}

func (r *Runtime) List(ctx context.Context) ([]db.AutomationScript, error) {
	return r.repository.List(ctx)
}

func (r *Runtime) Get(ctx context.Context, id string) (db.AutomationScript, error) {
	return r.repository.Get(ctx, id)
}

func (r *Runtime) Validate(ctx context.Context, id, source string, timeoutMS int64) error {
	if timeoutMS == 0 {
		timeoutMS = defaultTimeoutMS
	}
	compileCtx, cancel := context.WithCancel(ctx)
	stopRuntimeCancel := context.AfterFunc(r.ctx, cancel)
	defer func() {
		stopRuntimeCancel()
		cancel()
	}()
	_, _, err := compileWithContext(compileCtx, id, source, timeoutMS)
	return err
}

func (r *Runtime) Save(ctx context.Context, definition db.AutomationScript, expectedRevision int64) (db.AutomationScript, error) {
	if definition.TimeoutMS == 0 {
		definition.TimeoutMS = defaultTimeoutMS
	}
	definition.RuntimeStatus = "READY"
	definition.LastError = ""
	compileCtx, cancel := context.WithCancel(ctx)
	stopRuntimeCancel := context.AfterFunc(r.ctx, cancel)
	defer func() {
		stopRuntimeCancel()
		cancel()
	}()
	program, compiled, err := compileWithContext(compileCtx, definition.ID, definition.Source, definition.TimeoutMS)
	if err != nil {
		return db.AutomationScript{}, err
	}
	if err := compileCtx.Err(); err != nil {
		return db.AutomationScript{}, err
	}
	r.mutationMu.Lock()
	defer r.mutationMu.Unlock()
	if r.ctx.Err() != nil {
		return db.AutomationScript{}, errors.New("automation runtime is stopping")
	}
	saved, err := r.repository.Save(ctx, definition, expectedRevision)
	if err != nil {
		return db.AutomationScript{}, err
	}
	r.mu.Lock()
	r.scripts[saved.ID] = &compiledScript{definition: saved, program: program, interval: compiled.interval, message: compiled.messageEntry, timer: compiled.timerEntry}
	r.mu.Unlock()
	r.nudgeTimer()
	slog.Info("[Automation] script saved", "script_id", saved.ID, "revision", saved.Revision, "enabled", saved.Enabled, "status", saved.RuntimeStatus)
	return saved, nil
}

// SetEnabled changes only the desired state; a FROZEN or BROKEN script stays inactive until Recover or a source update.
func (r *Runtime) SetEnabled(ctx context.Context, id string, enabled bool, expectedRevision int64) (db.AutomationScript, error) {
	r.mutationMu.Lock()
	defer r.mutationMu.Unlock()
	if r.ctx.Err() != nil {
		return db.AutomationScript{}, errors.New("automation runtime is stopping")
	}
	current, err := r.repository.Get(ctx, id)
	if err != nil {
		return db.AutomationScript{}, err
	}
	current.Enabled = enabled
	// The resident status wins: it may be FROZEN even when persisting that failed.
	r.mu.RLock()
	if script, ok := r.scripts[id]; ok {
		current.RuntimeStatus = script.definition.RuntimeStatus
		current.LastError = script.definition.LastError
	}
	r.mu.RUnlock()
	saved, err := r.repository.Save(ctx, current, expectedRevision)
	if err != nil {
		return db.AutomationScript{}, err
	}
	r.mu.Lock()
	if script, ok := r.scripts[id]; ok {
		updated := *script
		updated.definition = saved
		r.scripts[id] = &updated
	}
	r.mu.Unlock()
	r.nudgeTimer()
	slog.Info("[Automation] script enabled state changed", "script_id", id, "revision", saved.Revision, "enabled", enabled, "status", saved.RuntimeStatus)
	return saved, nil
}

// Recover recompiles the stored source and returns the script to READY; a positive timeoutMS replaces its timeout.
func (r *Runtime) Recover(ctx context.Context, id string, timeoutMS, expectedRevision int64) (db.AutomationScript, error) {
	current, err := r.repository.Get(ctx, id)
	if err != nil {
		return db.AutomationScript{}, err
	}
	if current.Revision != expectedRevision {
		return db.AutomationScript{}, db.ErrAutomationConflict
	}
	if timeoutMS > 0 {
		current.TimeoutMS = timeoutMS
	}
	return r.Save(ctx, current, expectedRevision)
}

func (r *Runtime) Delete(ctx context.Context, id string, expectedRevision int64) error {
	r.mutationMu.Lock()
	defer r.mutationMu.Unlock()
	if r.ctx.Err() != nil {
		return errors.New("automation runtime is stopping")
	}
	if err := r.repository.Delete(ctx, id, expectedRevision); err != nil {
		return err
	}
	r.mu.Lock()
	delete(r.scripts, id)
	r.mu.Unlock()
	r.nudgeTimer()
	slog.Info("[Automation] script deleted", "script_id", id)
	return nil
}

// compiledProgram is what compiling tells the runtime beyond the program itself.
type compiledProgram struct {
	interval     int64 // timer_interval_minutes value, 0 = none
	messageEntry bool  // on_group_text is defined and callable
	timerEntry   bool  // on_timer is defined and callable
}

func compile(id, source string, timeoutMS int64) (*starlark.Program, compiledProgram, error) {
	return compileWithContext(context.Background(), id, source, timeoutMS)
}

func compileWithContext(parent context.Context, id, source string, timeoutMS int64) (*starlark.Program, compiledProgram, error) {
	if timeoutMS <= 0 {
		return nil, compiledProgram{}, errors.New("timeout_ms must be positive")
	}
	file, program, err := starlark.SourceProgram(id, source, isPredeclared)
	if err != nil {
		return nil, compiledProgram{}, err
	}
	interval, err := parseTimerInterval(file)
	if err != nil {
		return nil, compiledProgram{}, err
	}
	ctx, cancel := context.WithTimeout(parent, timeoutDuration(timeoutMS))
	defer cancel()
	thread := &starlark.Thread{Name: id + "-validation"}
	stopInterrupt := context.AfterFunc(ctx, func() { thread.Cancel("script validation timed out") })
	defer stopInterrupt()
	globals, err := program.Init(thread, validationBuiltins())
	if err != nil {
		if ctx.Err() != nil {
			return nil, compiledProgram{}, fmt.Errorf("script initialization exceeded timeout: %w", ctx.Err())
		}
		return nil, compiledProgram{}, err
	}
	compiled := compiledProgram{interval: interval}
	_, compiled.messageEntry = globals["on_group_text"].(starlark.Callable)
	_, compiled.timerEntry = globals["on_timer"].(starlark.Callable)
	if err := checkEntryPoints(compiled); err != nil {
		return nil, compiledProgram{}, err
	}
	return program, compiled, nil
}

// checkEntryPoints enforces the entry-point rules after the module's globals exist.
func checkEntryPoints(compiled compiledProgram) error {
	switch {
	case compiled.interval > 0 && !compiled.timerEntry:
		return errors.New("script sets timer_interval_minutes but does not define callable on_timer(message)")
	case compiled.timerEntry && compiled.interval == 0:
		return errors.New("script defines on_timer but does not set timer_interval_minutes")
	case !compiled.messageEntry && !compiled.timerEntry:
		return errors.New("script must define callable on_group_text(message) or a timer")
	}
	return nil
}

// predeclaredValues builds the table a script sees: the library modules plus one
// wrapped value per host function, so validation and invocation cannot drift.
func predeclaredValues(wrap func(name string, function hostFunction) starlark.Value) starlark.StringDict {
	values := make(starlark.StringDict, len(hostFunctions)+len(predeclaredModules))
	for name, value := range predeclaredModules {
		values[name] = value
	}
	for name, function := range hostFunctions {
		values[name] = wrap(name, function)
	}
	return values
}

// validationBuiltins are the compile-time values: a module-level host call must
// parse and resolve, not act, so host functions are stubs and libraries are real.
func validationBuiltins() starlark.StringDict {
	return predeclaredValues(func(name string, _ hostFunction) starlark.Value {
		return starlark.NewBuiltin(name, func(*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple) (starlark.Value, error) {
			return starlark.None, nil
		})
	})
}

// parseTimerInterval reads timer_interval_minutes off the module top level: a
// plain integer literal, or absent/0 for "no timer".
func parseTimerInterval(file *syntax.File) (int64, error) {
	interval := int64(0)
	for _, statement := range file.Stmts {
		assignment, ok := statement.(*syntax.AssignStmt)
		if !ok || assignment.Op != syntax.EQ {
			continue
		}
		name, ok := assignment.LHS.(*syntax.Ident)
		if !ok || name.Name != "timer_interval_minutes" {
			continue
		}
		value := int64(0)
		switch literal := assignment.RHS.(type) {
		case *syntax.Literal:
			if literal.Token != syntax.INT {
				return 0, errors.New("timer_interval_minutes must be an integer literal number")
			}
			int64Value, ok := literal.Value.(int64)
			if !ok {
				return 0, errors.New("timer_interval_minutes must be an integer literal number")
			}
			value = int64Value
		case *syntax.UnaryExpr:
			// Only -N is a literal in the sense that matters here.
			operand, isLiteral := literal.X.(*syntax.Literal)
			if literal.Op != syntax.MINUS || !isLiteral || operand.Token != syntax.INT {
				return 0, errors.New("timer_interval_minutes must be an integer literal number")
			}
			int64Value, ok := operand.Value.(int64)
			if !ok {
				return 0, errors.New("timer_interval_minutes must be an integer literal number")
			}
			value = -int64Value
		default:
			return 0, errors.New("timer_interval_minutes must be an integer literal number")
		}
		// 0 is "no timer"; anything shorter than the floor (including negatives)
		// would be a tight loop.
		if value != 0 && time.Duration(value)*time.Minute < minTimerInterval {
			return 0, errors.New("timer_interval_minutes must be at least 1")
		}
		interval = value
	}
	return interval, nil
}

func timeoutDuration(timeoutMS int64) time.Duration {
	maximumMS := int64(math.MaxInt64 / int64(time.Millisecond))
	if timeoutMS > maximumMS {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(timeoutMS) * time.Millisecond
}

func toStarlarkMessage(message parsers.GroupTextMessage) starlark.Value {
	path := make([]starlark.Value, 0, len(message.HopStations))
	for _, station := range message.HopStations {
		path = append(path, starlark.String(fmt.Sprintf("%x", append([]byte(nil), station...))))
	}
	fields := map[string]starlark.Value{
		"channel":            starlark.String(normalizeChannel(message.ChannelName)),
		"sender":             starlark.String(message.Sender),
		"body":               starlark.String(message.Body),
		"timestamp_unix":     starlark.MakeInt64(message.Timestamp.Unix()),
		"hop_count":          starlark.MakeInt(message.HopCount),
		"hop_stations":       starlark.NewList(path),
		"has_rx_meta":        starlark.Bool(message.HasRxMeta),
		"snr_quarter_db":     starlark.MakeInt(message.SNR),
		"rssi_dbm":           starlark.MakeInt(message.RSSI),
		"time_drift_seconds": starlark.MakeInt64(message.TimeDriftSeconds),
	}
	result := starlark.NewDict(len(fields))
	for key, value := range fields {
		_ = result.SetKey(starlark.String(key), value)
	}
	result.Freeze()
	return result
}

func cloneMessage(message parsers.GroupTextMessage) parsers.GroupTextMessage {
	message.PublicKeyPrefix = append([]byte(nil), message.PublicKeyPrefix...)
	stations := make([][]byte, len(message.HopStations))
	for index, station := range message.HopStations {
		stations[index] = append([]byte(nil), station...)
	}
	message.HopStations = stations
	return message
}

func normalizeChannel(channel string) string {
	channel = strings.ToLower(strings.TrimSpace(channel))
	if channel != "" && !strings.HasPrefix(channel, "#") {
		channel = "#" + channel
	}
	return channel
}
