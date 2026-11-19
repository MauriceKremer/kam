package automation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.starlark.net/starlark"
	"kam.bytecoder.nl/internal/db"
	"kam.bytecoder.nl/internal/kiss/parsers"
)

const defaultDryRunTimeoutMS = int64(500)
const maxDryRunTimeoutMS = int64(3000)
const maxDryRunReplies = 16
const maxDryRunReplyBytes = 4096

// ErrDryRunBusy reports that another isolated script test is running.
var ErrDryRunBusy = errors.New("another script dry run is already running")

type replyValidator interface {
	ValidateReply(string, string) error
}

// DryRunReply contains one reply captured by a dry-run invocation.
type DryRunReply struct {
	Channel string
	Text    string
}

// DryRunResult reports whether a test passed and the replies it captured.
type DryRunResult struct {
	OK         bool
	DurationMS int64
	Replies    []DryRunReply
	Error      string
}

// DryRun compiles and invokes one source against a fixture without changing
// persisted or resident script state and without using the live sender.
func (r *Runtime) DryRun(ctx context.Context, id, source string, timeoutMS int64, message parsers.GroupTextMessage) (DryRunResult, error) {
	if !r.dryRunMu.TryLock() {
		return DryRunResult{}, ErrDryRunBusy
	}
	defer r.dryRunMu.Unlock()

	if timeoutMS == 0 {
		timeoutMS = defaultDryRunTimeoutMS
	}
	if timeoutMS < 1 || timeoutMS > maxDryRunTimeoutMS {
		return DryRunResult{}, errors.New("timeout_ms must be between 1 and 3000")
	}
	_, _, channel, ok := parsers.LookupTopic(message.ChannelName)
	if !ok {
		return DryRunResult{}, fmt.Errorf("message.channel %q is not an enabled channel", message.ChannelName)
	}
	message.ChannelName = channel
	if message.Timestamp.IsZero() {
		message.Timestamp = time.Now().UTC()
	}
	if message.HopCount < 0 {
		return DryRunResult{}, errors.New("message.hop_count must not be negative")
	}
	if len(message.HopStations) > 63 {
		return DryRunResult{}, errors.New("message.hop_stations must contain at most 63 stations")
	}
	for _, station := range message.HopStations {
		if len(station) < 1 || len(station) > 3 {
			return DryRunResult{}, errors.New("message.hop_stations entries must be 1 to 3 bytes")
		}
	}

	started := time.Now()
	result := DryRunResult{Replies: []DryRunReply{}}
	runContext, cancel := context.WithTimeout(ctx, timeoutDuration(timeoutMS))
	stopRuntimeCancel := context.AfterFunc(r.ctx, cancel)
	defer func() {
		stopRuntimeCancel()
		cancel()
	}()
	deadline, _ := runContext.Deadline()
	remaining := time.Until(deadline)
	if remaining <= 0 {
		result.Error = "dry run timed out"
		result.DurationMS = time.Since(started).Milliseconds()
		return result, nil
	}
	remainingMS := int64((remaining + time.Millisecond - 1) / time.Millisecond)
	program, compiled, err := compileWithContext(runContext, id, source, remainingMS)
	if err == nil && !compiled.messageEntry {
		err = errors.New("dry run needs on_group_text(message); timer-only scripts cannot be tested with a message fixture")
	}
	if err != nil {
		result.Error = err.Error()
		result.DurationMS = time.Since(started).Milliseconds()
		return result, nil
	}

	validator, _ := r.sender.(replyValidator)
	captured := &capturedReplySender{validator: validator}
	script := &compiledScript{
		definition: db.AutomationScript{ID: id, Source: source, TimeoutMS: timeoutMS, RuntimeStatus: "READY"},
		program:    program,
		interval:   compiled.interval,
		schedule:   compiled.schedule,
		spec:       compiled.spec,
		message:    compiled.messageEntry,
		timer:      compiled.timerEntry,
	}
	err = r.invokeWithSender(runContext, script, "on_group_text", starlark.Tuple{toStarlarkMessage(message)}, captured, timeoutDuration(timeoutMS))
	result.Replies = captured.replies
	result.DurationMS = time.Since(started).Milliseconds()
	if err != nil {
		result.Error = err.Error()
		return result, nil
	}
	result.OK = true
	return result, nil
}

type capturedReplySender struct {
	validator replyValidator
	replies   []DryRunReply
	bytes     int
}

func (s *capturedReplySender) Send(ctx context.Context, channel, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, _, normalized, ok := parsers.LookupTopic(channel)
	if !ok {
		return fmt.Errorf("unknown channel %q", channel)
	}
	if len(s.replies) >= maxDryRunReplies {
		return errors.New("dry run exceeded the 16 reply limit")
	}
	replyBytes := len([]byte(normalized)) + len([]byte(text))
	if s.bytes+replyBytes > maxDryRunReplyBytes {
		return errors.New("dry run exceeded the 4096-byte reply limit")
	}
	if s.validator != nil {
		if err := s.validator.ValidateReply(normalized, text); err != nil {
			return err
		}
	}
	s.replies = append(s.replies, DryRunReply{Channel: normalized, Text: text})
	s.bytes += replyBytes
	return nil
}
