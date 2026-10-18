package automation

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"kam.bytecoder.nl/internal/db"
)

const LocalAPIAddress = "127.0.0.1:8765"

// apiEndpoint is one row of the self-describing index served at GET /.
type apiEndpoint struct {
	Method  string `json:"method"`
	Path    string `json:"path"`
	Purpose string `json:"purpose"`
}

const defaultTimeoutMS = int64(db.DefaultAutomationTimeout / time.Millisecond)

// NewAPI returns the Local Management API handler. The API is unauthenticated, so it only serves loopback
// Host headers and JSON bodies; that stops web pages from driving it via DNS rebinding or cross-site forms.
func NewAPI(runtime *Runtime) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		endpoints := []apiEndpoint{
			{Method: "GET", Path: "/scripts", Purpose: "list all scripts"},
			{Method: "POST", Path: "/scripts", Purpose: "create a script (expected_revision 0)"},
			{Method: "GET", Path: "/scripts/{id}", Purpose: "read one script"},
			{Method: "PUT", Path: "/scripts/{id}", Purpose: "update a script (expected_revision)"},
			{Method: "DELETE", Path: "/scripts/{id}?revision=N", Purpose: "delete a script"},
			{Method: "POST", Path: "/scripts/validate", Purpose: "compile source without saving"},
			{Method: "POST", Path: "/scripts/{id}/enable", Purpose: "enable the desired state (expected_revision)"},
			{Method: "POST", Path: "/scripts/{id}/disable", Purpose: "disable the desired state (expected_revision)"},
			{Method: "POST", Path: "/scripts/{id}/recover", Purpose: "recompile a FROZEN or BROKEN script"},
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"api":       "kam",
			"endpoints": endpoints,
			"docs":      "docs/APIdetails.md",
		})
	})
	mux.HandleFunc("GET /scripts", func(w http.ResponseWriter, r *http.Request) {
		scripts, err := runtime.repository.List(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, scripts)
	})
	mux.HandleFunc("POST /scripts/validate", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID        string `json:"id"`
			Source    string `json:"source"`
			TimeoutMS int64  `json:"timeout_ms"`
		}
		if !decodeJSON(w, r, &request) {
			return
		}
		if request.TimeoutMS == 0 {
			request.TimeoutMS = defaultTimeoutMS
		}
		if request.ID == "" {
			request.ID = "validation"
		}
		if err := runtime.Validate(r.Context(), request.ID, request.Source, request.TimeoutMS); err != nil {
			writeError(w, http.StatusUnprocessableEntity, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"valid": true})
	})
	mux.HandleFunc("POST /scripts", func(w http.ResponseWriter, r *http.Request) {
		var request scriptMutation
		if !decodeJSON(w, r, &request) {
			return
		}
		if request.Script.TimeoutMS == 0 {
			request.Script.TimeoutMS = defaultTimeoutMS
		}
		saved, err := runtime.Save(r.Context(), request.Script, 0)
		if err != nil {
			writeMutationError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, saved)
	})
	mux.HandleFunc("GET /scripts/{id}", func(w http.ResponseWriter, r *http.Request) {
		script, err := runtime.Get(r.Context(), r.PathValue("id"))
		if err != nil {
			writeMutationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, script)
	})
	mux.HandleFunc("PUT /scripts/{id}", func(w http.ResponseWriter, r *http.Request) {
		var request scriptMutation
		if !decodeJSON(w, r, &request) {
			return
		}
		request.Script.ID = r.PathValue("id")
		saved, err := runtime.Save(r.Context(), request.Script, request.ExpectedRevision)
		if err != nil {
			writeMutationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, saved)
	})
	mux.HandleFunc("DELETE /scripts/{id}", func(w http.ResponseWriter, r *http.Request) {
		revision, err := strconv.ParseInt(r.URL.Query().Get("revision"), 10, 64)
		if err != nil || revision <= 0 {
			writeError(w, http.StatusBadRequest, errors.New("positive revision query parameter is required"))
			return
		}
		if err := runtime.Delete(r.Context(), r.PathValue("id"), revision); err != nil {
			writeMutationError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	for _, action := range []string{"enable", "disable", "recover"} {
		mux.HandleFunc("POST /scripts/{id}/"+action, func(w http.ResponseWriter, r *http.Request) {
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
			var saved db.AutomationScript
			var err error
			if action == "recover" {
				saved, err = runtime.Recover(r.Context(), r.PathValue("id"), request.TimeoutMS, request.ExpectedRevision)
			} else {
				saved, err = runtime.SetEnabled(r.Context(), r.PathValue("id"), action == "enable", request.ExpectedRevision)
			}
			if err != nil {
				writeMutationError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, saved)
		})
	}
	return requireLocalJSON(mux)
}

// requireLocalJSON rejects non-loopback Host headers (DNS rebinding) and non-JSON bodies. A JSON content
// type cannot be sent cross-site without a CORS preflight, which this API never grants.
func requireLocalJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackHost(r.Host) {
			writeError(w, http.StatusForbidden, errors.New("Local Management API only serves loopback hosts"))
			return
		}
		if r.Method == http.MethodPost || r.Method == http.MethodPut {
			if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/json" {
				writeError(w, http.StatusUnsupportedMediaType, errors.New("Content-Type must be application/json"))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address, err := netip.ParseAddr(host)
	return err == nil && address.IsLoopback()
}

func ListenLocalAPI() (net.Listener, error) {
	return listenLoopback(LocalAPIAddress)
}

func listenLoopback(address string) (net.Listener, error) {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	host, _, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	bound, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil || !bound.IsLoopback() {
		_ = listener.Close()
		return nil, errors.New("Local Management API must bind to a loopback address")
	}
	return listener, nil
}

func ServeLocalAPI(ctx context.Context, runtime *Runtime) (*http.Server, error) {
	listener, err := ListenLocalAPI()
	if err != nil {
		return nil, err
	}
	server := &http.Server{Handler: NewAPI(runtime), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("[Automation] Local Management API stopped", "error", err)
		}
	}()
	return server, nil
}

type scriptMutation struct {
	Script           db.AutomationScript `json:"script"`
	ExpectedRevision int64               `json:"expected_revision"`
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func writeMutationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, db.ErrAutomationNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, db.ErrAutomationConflict), errors.Is(err, db.ErrAutomationNameTaken):
		writeError(w, http.StatusConflict, err)
	default:
		writeError(w, http.StatusUnprocessableEntity, err)
	}
}
