// The adapter that serves the API's Scripts seam from the automation runtime.
// It maps the capability's stored rows into api.Script and wraps its domain
// sentinels into the transport's error classes; unclassified errors pass
// through and map to 422 (D6, D8). See docs/FeatureUnifiedApi.md.
package node

import (
	"context"
	"errors"

	"kam.bytecoder.nl/internal/api"
	"kam.bytecoder.nl/internal/automation"
	"kam.bytecoder.nl/internal/db"
	"kam.bytecoder.nl/internal/kiss/parsers"
)

type apiScripts struct {
	runtime *automation.Runtime
}

func (s apiScripts) List(ctx context.Context) ([]api.Script, error) {
	stored, err := s.runtime.List(ctx)
	if err != nil {
		return nil, err
	}
	intervals := s.runtime.TimerIntervals()
	schedules := s.runtime.TimerSchedules()
	scripts := make([]api.Script, 0, len(stored))
	for _, item := range stored {
		scripts = append(scripts, withPeriods(toAPIScript(item), intervals, schedules))
	}
	return scripts, nil
}

func (s apiScripts) Get(ctx context.Context, id string) (api.Script, error) {
	script, err := s.runtime.Get(ctx, id)
	if err != nil {
		return api.Script{}, translateScriptError(err)
	}
	return withPeriods(toAPIScript(script), s.runtime.TimerIntervals(), s.runtime.TimerSchedules()), nil
}

func (s apiScripts) Save(ctx context.Context, script api.Script, expectedRevision int64) (api.Script, error) {
	saved, err := s.runtime.Save(ctx, toDBScript(script), expectedRevision)
	if err != nil {
		return api.Script{}, translateScriptError(err)
	}
	return withPeriods(toAPIScript(saved), s.runtime.TimerIntervals(), s.runtime.TimerSchedules()), nil
}

func (s apiScripts) Delete(ctx context.Context, id string, revision int64) error {
	return translateScriptError(s.runtime.Delete(ctx, id, revision))
}

func (s apiScripts) SetEnabled(ctx context.Context, id string, enabled bool, expectedRevision int64) (api.Script, error) {
	saved, err := s.runtime.SetEnabled(ctx, id, enabled, expectedRevision)
	if err != nil {
		return api.Script{}, translateScriptError(err)
	}
	return withPeriods(toAPIScript(saved), s.runtime.TimerIntervals(), s.runtime.TimerSchedules()), nil
}

func (s apiScripts) Recover(ctx context.Context, id string, timeoutMS int64, expectedRevision int64) (api.Script, error) {
	saved, err := s.runtime.Recover(ctx, id, timeoutMS, expectedRevision)
	if err != nil {
		return api.Script{}, translateScriptError(err)
	}
	return withPeriods(toAPIScript(saved), s.runtime.TimerIntervals(), s.runtime.TimerSchedules()), nil
}

func (s apiScripts) Validate(ctx context.Context, id, source string, timeoutMS int64) error {
	return s.runtime.Validate(ctx, id, source, timeoutMS)
}

func (s apiScripts) DryRun(ctx context.Context, request api.ScriptDryRunRequest) (api.ScriptDryRunResult, error) {
	message := request.Message
	result, err := s.runtime.DryRun(ctx, "dry-run", request.Source, request.TimeoutMS, parsers.GroupTextMessage{
		ChannelName:      message.Channel,
		Timestamp:        message.Timestamp,
		Message:          message.Body,
		Sender:           message.Sender,
		Body:             message.Body,
		HopCount:         message.HopCount,
		HopStations:      message.HopStations,
		HasRxMeta:        message.HasRxMeta,
		SNR:              message.SNRQuarterDB,
		RSSI:             message.RSSIDBm,
		TimeDriftSeconds: message.TimeDriftSeconds,
	})
	if errors.Is(err, automation.ErrDryRunBusy) {
		return api.ScriptDryRunResult{}, &api.ScriptError{Class: api.ScriptErrorConflict, Message: err.Error(), Cause: err}
	}
	if err != nil {
		return api.ScriptDryRunResult{}, translateScriptError(err)
	}
	replies := make([]api.ScriptDryRunReply, 0, len(result.Replies))
	for _, reply := range result.Replies {
		replies = append(replies, api.ScriptDryRunReply{Channel: reply.Channel, Text: reply.Text})
	}
	return api.ScriptDryRunResult{OK: result.OK, DurationMS: result.DurationMS, Replies: replies, Error: result.Error}, nil
}

// translateScriptError classifies the runtime's domain sentinels for the
// transport; the message the wire shows stays the capability's own text.
func translateScriptError(err error) error {
	var class api.ScriptErrorClass
	switch {
	case errors.Is(err, automation.ErrScriptNotFound):
		class = api.ScriptErrorNotFound
	case errors.Is(err, automation.ErrScriptConflict), errors.Is(err, automation.ErrScriptNameTaken):
		class = api.ScriptErrorConflict
	default:
		return err
	}
	return &api.ScriptError{Class: class, Message: err.Error(), Cause: err}
}

// withPeriods fills the read-only timer fields from the compiled source.
func withPeriods(script api.Script, intervals map[string]int64, schedules map[string]string) api.Script {
	script.TimerIntervalMinutes = intervals[script.ID]
	script.TimerSchedule = schedules[script.ID]
	return script
}

func toAPIScript(script db.AutomationScript) api.Script {
	return api.Script{
		ID: script.ID, Name: script.Name, Source: script.Source, Enabled: script.Enabled,
		TimeoutMS: script.TimeoutMS, RuntimeStatus: script.RuntimeStatus, LastError: script.LastError,
		Revision: script.Revision, CreatedAt: script.CreatedAt, UpdatedAt: script.UpdatedAt,
	}
}

func toDBScript(script api.Script) db.AutomationScript {
	return db.AutomationScript{
		ID: script.ID, Name: script.Name, Source: script.Source, Enabled: script.Enabled,
		TimeoutMS: script.TimeoutMS, RuntimeStatus: script.RuntimeStatus, LastError: script.LastError,
		Revision: script.Revision, CreatedAt: script.CreatedAt, UpdatedAt: script.UpdatedAt,
	}
}
