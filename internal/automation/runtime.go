package automation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.starlark.net/starlark"
	"kam.bytecoder.nl/internal/db"
	"kam.bytecoder.nl/internal/kiss/parsers"
)

const defaultQueueSize = 128
const defaultReplyTimeout = 500 * time.Millisecond

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
}

// hostFunction is a capability scripts may call; ctx carries the invocation deadline and must be honoured.
type hostFunction func(ctx context.Context, r *Runtime, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error)

// hostFunctions is the complete set of names visible to scripts; anything else fails compilation.
var hostFunctions = map[string]hostFunction{"reply": hostReply}

func hostReply(ctx context.Context, r *Runtime, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var channel, text string
	if err := starlark.UnpackArgs("reply", args, kwargs, "channel", &channel, "text", &text); err != nil {
		return starlark.None, err
	}
	sendContext, cancel := context.WithTimeout(ctx, defaultReplyTimeout)
	defer cancel()
	return starlark.None, r.sender.Send(sendContext, channel, text)
}

type Runtime struct {
	repository Repository
	sender     Sender
	queue      chan parsers.GroupTextMessage
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}

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

func NewRuntime(parent context.Context, repository Repository, sender Sender) (*Runtime, error) {
	return NewRuntimeWithQueueSize(parent, repository, sender, defaultQueueSize)
}

func NewRuntimeWithQueueSize(parent context.Context, repository Repository, sender Sender, queueSize int) (*Runtime, error) {
	if queueSize <= 0 {
		return nil, errors.New("automation queue size must be positive")
	}
	definitions, err := repository.List(parent)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	runtime := &Runtime{
		repository: repository,
		sender:     sender,
		queue:      make(chan parsers.GroupTextMessage, queueSize),
		ctx:        ctx,
		cancel:     cancel,
		done:       make(chan struct{}),
		scripts:    make(map[string]*compiledScript, len(definitions)),
	}
	for _, definition := range definitions {
		program, err := compile(definition.ID, definition.Source, definition.TimeoutMS)
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
			runtime.scripts[definition.ID] = &compiledScript{definition: definition, program: program}
		}
	}
	go runtime.worker()
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
	case r.queue <- copy:
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
		case message := <-r.queue:
			if r.ctx.Err() != nil {
				r.dropQueuedEvents(1)
				return
			}
			r.dispatch(message)
		}
	}
}

func (r *Runtime) dropQueuedEvents(alreadyDropped int) {
	dropped := alreadyDropped
	for {
		select {
		case <-r.queue:
			dropped++
		default:
			if dropped > 0 {
				slog.Info("[Automation] queued group-text events dropped during shutdown", "count", dropped)
			}
			return
		}
	}
}

func (r *Runtime) dispatch(message parsers.GroupTextMessage) {
	r.mu.RLock()
	snapshot := make([]*compiledScript, 0, len(r.scripts))
	for _, script := range r.scripts {
		if script.definition.Enabled && script.definition.RuntimeStatus == "READY" {
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
		if err := r.invoke(script, message); err != nil {
			if errors.Is(err, errScriptTimeout) {
				r.freeze(script, err)
			} else if r.ctx.Err() == nil {
				slog.Warn("[Automation] script invocation failed", "script_id", script.definition.ID, "error", err)
			}
		}
	}
}

func (r *Runtime) invoke(script *compiledScript, message parsers.GroupTextMessage) error {
	timeout := timeoutDuration(script.definition.TimeoutMS)
	ctx, cancel := context.WithTimeout(r.ctx, timeout)
	defer cancel()
	thread := &starlark.Thread{Name: script.definition.ID}
	stopInterrupt := context.AfterFunc(ctx, func() { thread.Cancel("script execution interrupted") })
	defer stopInterrupt()
	predeclared := make(starlark.StringDict, len(hostFunctions))
	for name, function := range hostFunctions {
		predeclared[name] = starlark.NewBuiltin(name, func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			return function(ctx, r, args, kwargs)
		})
	}
	globals, err := script.program.Init(thread, predeclared)
	if err != nil {
		return classifyInvokeError(ctx, timeout, err)
	}
	handler, ok := globals["on_group_text"]
	if !ok {
		return errors.New("script does not define on_group_text")
	}
	if _, err := starlark.Call(thread, handler, starlark.Tuple{toStarlarkMessage(message)}, nil); err != nil {
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
	_, err := compileWithContext(compileCtx, id, source, timeoutMS)
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
	program, err := compileWithContext(compileCtx, definition.ID, definition.Source, definition.TimeoutMS)
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
	r.scripts[saved.ID] = &compiledScript{definition: saved, program: program}
	r.mu.Unlock()
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
	slog.Info("[Automation] script deleted", "script_id", id)
	return nil
}

func compile(id, source string, timeoutMS int64) (*starlark.Program, error) {
	return compileWithContext(context.Background(), id, source, timeoutMS)
}

func compileWithContext(parent context.Context, id, source string, timeoutMS int64) (*starlark.Program, error) {
	if timeoutMS <= 0 {
		return nil, errors.New("timeout_ms must be positive")
	}
	_, program, err := starlark.SourceProgram(id, source, func(name string) bool {
		_, ok := hostFunctions[name]
		return ok
	})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(parent, timeoutDuration(timeoutMS))
	defer cancel()
	thread := &starlark.Thread{Name: id + "-validation"}
	stopInterrupt := context.AfterFunc(ctx, func() { thread.Cancel("script validation timed out") })
	defer stopInterrupt()
	stubs := make(starlark.StringDict, len(hostFunctions))
	for name := range hostFunctions {
		stubs[name] = starlark.NewBuiltin(name, func(*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple) (starlark.Value, error) {
			return starlark.None, nil
		})
	}
	globals, err := program.Init(thread, stubs)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("script initialization exceeded timeout: %w", ctx.Err())
		}
		return nil, err
	}
	if _, ok := globals["on_group_text"].(starlark.Callable); !ok {
		return nil, errors.New("script must define callable on_group_text(message)")
	}
	return program, nil
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
