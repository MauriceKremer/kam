// Script routes and the automation seam for this package: the handler is
// transport only, so every script operation goes through Scripts, implemented
// by an adapter in internal/node that maps the capability's stored rows and
// domain sentinels into these shapes (D6, D8). See docs/FeatureUnifiedApi.md.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
)

// Script is the script resource's wire shape; node's adapter maps it from the
// capability's AutomationScript.
type Script struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Source        string `json:"source"`
	Enabled       bool   `json:"enabled"`
	TimeoutMS     int64  `json:"timeout_ms"`
	RuntimeStatus string `json:"runtime_status"`
	LastError     string `json:"last_error,omitempty"`
	// TimerIntervalMinutes is the period the script's source declares. It is
	// read-only: the source decides it, so a request's value is ignored.
	TimerIntervalMinutes int64 `json:"timer_interval_minutes"`
	Revision             int64 `json:"revision"`
	CreatedAt            int64 `json:"created_at"`
	UpdatedAt            int64 `json:"updated_at"`
}

// ScriptMutation wraps a script write with the revision the caller loaded.
// A create ignores expected_revision: revision 0 is what a create means.
type ScriptMutation struct {
	Script           Script `json:"script"`
	ExpectedRevision int64  `json:"expected_revision"`
}

// ScriptValidateRequest is the body of POST /scripts/validate, which compiles
// source without saving; an empty ID compiles under "validation".
type ScriptValidateRequest struct {
	ID        string `json:"id"`
	Source    string `json:"source"`
	TimeoutMS int64  `json:"timeout_ms"`
}

// Scripts is the automation capability seam: script management plus the
// compile-only validation. Implementations apply capability policy (timeouts,
// revisions) and classify their failures with ScriptError.
type Scripts interface {
	List(context.Context) ([]Script, error)
	Get(ctx context.Context, id string) (Script, error)
	Save(ctx context.Context, script Script, expectedRevision int64) (Script, error)
	Delete(ctx context.Context, id string, revision int64) error
	SetEnabled(ctx context.Context, id string, enabled bool, expectedRevision int64) (Script, error)
	Recover(ctx context.Context, id string, timeoutMS int64, expectedRevision int64) (Script, error)
	Validate(ctx context.Context, id, source string, timeoutMS int64) error
}

// ScriptErrorClass is the transport's error vocabulary: the status code a
// Scripts failure maps to. The capability decides the class, the handler
// decides the code.
type ScriptErrorClass int

const (
	// ScriptErrorInvalid is the default for unclassified failures: compile
	// rejections and other capability refusals (422).
	ScriptErrorInvalid ScriptErrorClass = iota
	// ScriptErrorNotFound maps to 404.
	ScriptErrorNotFound
	// ScriptErrorConflict maps to 409: a stale revision or a name in use.
	ScriptErrorConflict
)

// ScriptError carries a Scripts failure to the handler: the class that maps
// to a status code and the exact message the response body shows.
type ScriptError struct {
	Class   ScriptErrorClass
	Message string
	Cause   error
}

func (e *ScriptError) Error() string { return e.Message }
func (e *ScriptError) Unwrap() error { return e.Cause }

// writeScriptError maps a Scripts failure to its status code. The body keeps
// the capability's own message; unclassified errors are invalid (422).
func writeScriptError(w http.ResponseWriter, err error) {
	class := ScriptErrorInvalid
	var scriptErr *ScriptError
	if errors.As(err, &scriptErr) {
		class = scriptErr.Class
	}
	switch class {
	case ScriptErrorNotFound:
		writeError(w, http.StatusNotFound, err)
	case ScriptErrorConflict:
		writeError(w, http.StatusConflict, err)
	default:
		writeError(w, http.StatusUnprocessableEntity, err)
	}
}

// decodeJSON reads one body with unknown-field rejection.
func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// automationDisabled is the body a script route serves while automation is
// off: the routes stay registered and keep port discovery stable (D9).
func automationDisabled(w http.ResponseWriter) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "automation is disabled"})
}

// registerScriptRoutes registers the script surface; it exists even when no
// Scripts source is wired, and answers 503 on every route in that case.
func registerScriptRoutes(mux *http.ServeMux, scripts Scripts) {
	mux.HandleFunc("GET /scripts", func(w http.ResponseWriter, r *http.Request) {
		if scripts == nil {
			automationDisabled(w)
			return
		}
		items, err := scripts.List(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if items == nil {
			items = []Script{}
		}
		writeJSON(w, http.StatusOK, items)
	})
	mux.HandleFunc("POST /scripts/validate", func(w http.ResponseWriter, r *http.Request) {
		if scripts == nil {
			automationDisabled(w)
			return
		}
		var request ScriptValidateRequest
		if !decodeJSON(w, r, &request) {
			return
		}
		if request.ID == "" {
			request.ID = "validation"
		}
		if err := scripts.Validate(r.Context(), request.ID, request.Source, request.TimeoutMS); err != nil {
			writeError(w, http.StatusUnprocessableEntity, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"valid": true})
	})
	mux.HandleFunc("POST /scripts", func(w http.ResponseWriter, r *http.Request) {
		if scripts == nil {
			automationDisabled(w)
			return
		}
		var request ScriptMutation
		if !decodeJSON(w, r, &request) {
			return
		}
		saved, err := scripts.Save(r.Context(), request.Script, 0)
		if err != nil {
			writeScriptError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, saved)
	})
	mux.HandleFunc("GET /scripts/{id}", func(w http.ResponseWriter, r *http.Request) {
		if scripts == nil {
			automationDisabled(w)
			return
		}
		script, err := scripts.Get(r.Context(), r.PathValue("id"))
		if err != nil {
			writeScriptError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, script)
	})
	mux.HandleFunc("PUT /scripts/{id}", func(w http.ResponseWriter, r *http.Request) {
		if scripts == nil {
			automationDisabled(w)
			return
		}
		var request ScriptMutation
		if !decodeJSON(w, r, &request) {
			return
		}
		request.Script.ID = r.PathValue("id")
		saved, err := scripts.Save(r.Context(), request.Script, request.ExpectedRevision)
		if err != nil {
			writeScriptError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, saved)
	})
	mux.HandleFunc("DELETE /scripts/{id}", func(w http.ResponseWriter, r *http.Request) {
		if scripts == nil {
			automationDisabled(w)
			return
		}
		revision, err := strconv.ParseInt(r.URL.Query().Get("revision"), 10, 64)
		if err != nil || revision <= 0 {
			writeError(w, http.StatusBadRequest, errors.New("positive revision query parameter is required"))
			return
		}
		if err := scripts.Delete(r.Context(), r.PathValue("id"), revision); err != nil {
			writeScriptError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	for _, action := range []string{"enable", "disable", "recover"} {
		mux.HandleFunc("POST /scripts/{id}/"+action, func(w http.ResponseWriter, r *http.Request) {
			if scripts == nil {
				automationDisabled(w)
				return
			}
			var request struct {
				ExpectedRevision int64 `json:"expected_revision"`
				TimeoutMS        int64 `json:"timeout_ms"`
			}
			if !decodeJSON(w, r, &request) {
				return
			}
			if request.ExpectedRevision <= 0 {
				writeError(w, http.StatusBadRequest, errors.New("positive expected_revision is required"))
				return
			}
			var saved Script
			var err error
			if action == "recover" {
				saved, err = scripts.Recover(r.Context(), r.PathValue("id"), request.TimeoutMS, request.ExpectedRevision)
			} else {
				saved, err = scripts.SetEnabled(r.Context(), r.PathValue("id"), action == "enable", request.ExpectedRevision)
			}
			if err != nil {
				writeScriptError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, saved)
		})
	}
}
