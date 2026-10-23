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
)

type apiScripts struct {
	runtime *automation.Runtime
}

func (s apiScripts) List(ctx context.Context) ([]api.Script, error) {
	stored, err := s.runtime.List(ctx)
	if err != nil {
		return nil, err
	}
	scripts := make([]api.Script, 0, len(stored))
	for _, item := range stored {
		scripts = append(scripts, toAPIScript(item))
	}
	return scripts, nil
}

func (s apiScripts) Get(ctx context.Context, id string) (api.Script, error) {
	script, err := s.runtime.Get(ctx, id)
	if err != nil {
		return api.Script{}, translateScriptError(err)
	}
	return toAPIScript(script), nil
}

func (s apiScripts) Save(ctx context.Context, script api.Script, expectedRevision int64) (api.Script, error) {
	saved, err := s.runtime.Save(ctx, toDBScript(script), expectedRevision)
	if err != nil {
		return api.Script{}, translateScriptError(err)
	}
	return toAPIScript(saved), nil
}

func (s apiScripts) Delete(ctx context.Context, id string, revision int64) error {
	return translateScriptError(s.runtime.Delete(ctx, id, revision))
}

func (s apiScripts) SetEnabled(ctx context.Context, id string, enabled bool, expectedRevision int64) (api.Script, error) {
	saved, err := s.runtime.SetEnabled(ctx, id, enabled, expectedRevision)
	if err != nil {
		return api.Script{}, translateScriptError(err)
	}
	return toAPIScript(saved), nil
}

func (s apiScripts) Recover(ctx context.Context, id string, timeoutMS int64, expectedRevision int64) (api.Script, error) {
	saved, err := s.runtime.Recover(ctx, id, timeoutMS, expectedRevision)
	if err != nil {
		return api.Script{}, translateScriptError(err)
	}
	return toAPIScript(saved), nil
}

func (s apiScripts) Validate(ctx context.Context, id, source string, timeoutMS int64) error {
	return s.runtime.Validate(ctx, id, source, timeoutMS)
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
