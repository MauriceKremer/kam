// Package api is KAM's local REST layer: routes, JSON decoding and shape
// validation, transport policy (loopback Host guard, JSON bodies, at most
// eight concurrent requests, a five-second deadline, panic recovery), the
// wire shapes of every response, error-to-status translation, and the
// self-describing GET / index.
//
// The package is transport only: it implements no KAM capability and imports
// nothing from this repository except internal/models (shared data types).
// Capabilities are handed in through the seams below — a scripts source, an
// events source, a status function, a channel reader and a room source — and
// are composed in internal/node. See docs/FeatureUnifiedApi.md.
package api

import (
	"encoding/json"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"kam.bytecoder.nl/internal/models"
)

// apiEndpoint is one row of the self-describing index served at GET /.
type apiEndpoint struct {
	Method  string `json:"method"`
	Path    string `json:"path"`
	Purpose string `json:"purpose"`
}

func NewHandler(ring EventsSource, status func() Status) http.Handler {
	return NewHandlerWithResources(ring, status, nil, nil, nil, nil, nil)
}

func NewHandlerWithChannels(ring EventsSource, status func() Status, channels ChannelReader) http.Handler {
	return NewHandlerWithResources(ring, status, nil, channels, nil, nil, nil)
}

// NewHandlerWithResources composes the whole listener: each area registers its
// routes on one mux (events.go, status.go, channels.go, rooms.go, scripts.go,
// modem.go), then the transport guards below wrap it. A nil seam switches its
// area off.
func NewHandlerWithResources(ring EventsSource, status func() Status, scripts Scripts, channels ChannelReader, rooms RoomSource, modem ModemSource, system func() *SystemStatus) http.Handler {
	mux := http.NewServeMux()
	registerScriptRoutes(mux, scripts)
	registerModemRoutes(mux, modem)
	registerEventRoutes(mux, ring)
	registerStatusRoutes(mux, ring, status, system)
	registerChannelRoutes(mux, channels)
	registerRoomRoutes(mux, rooms)
	indexHandler := func(w http.ResponseWriter, request *http.Request) {
		endpoints := []apiEndpoint{
			{Method: "GET", Path: "/v1/status", Purpose: "one snapshot of the running node; add ?system=true for machine memory, cpu, disk and database size"},
			{Method: "GET", Path: "/v1/events", Purpose: "event ring; poll with after, epoch, limit"},
		}
		if channels != nil {
			endpoints = append(endpoints,
				apiEndpoint{Method: "GET", Path: "/v1/channels", Purpose: "configured channels, never a key"},
				apiEndpoint{Method: "GET", Path: "/v1/channels/{name}/messages", Purpose: "stored group texts, newest first"},
			)
		}
		endpoints = append(endpoints,
			apiEndpoint{Method: "GET", Path: "/v1/rooms", Purpose: "rooms with counters; 503 when rooms are disabled"},
			apiEndpoint{Method: "GET", Path: "/v1/rooms/{id}/clients", Purpose: "remembered clients of one room"},
			apiEndpoint{Method: "GET", Path: "/v1/rooms/{id}/posts", Purpose: "retained posts, newest first"},
			apiEndpoint{Method: "DELETE", Path: "/v1/rooms/{id}/clients/{key}", Purpose: "forget one client's session and pending pushes; not a ban"},
			apiEndpoint{Method: "GET", Path: "/scripts", Purpose: "list all scripts"},
			apiEndpoint{Method: "POST", Path: "/scripts", Purpose: "create a script (expected_revision 0)"},
			apiEndpoint{Method: "GET", Path: "/scripts/{id}", Purpose: "read one script"},
			apiEndpoint{Method: "PUT", Path: "/scripts/{id}", Purpose: "update a script (expected_revision)"},
			apiEndpoint{Method: "DELETE", Path: "/scripts/{id}?revision=N", Purpose: "delete a script"},
			apiEndpoint{Method: "POST", Path: "/scripts/validate", Purpose: "compile source without saving"},
			apiEndpoint{Method: "POST", Path: "/scripts/{id}/enable", Purpose: "enable the desired state (expected_revision)"},
			apiEndpoint{Method: "POST", Path: "/scripts/{id}/disable", Purpose: "disable the desired state (expected_revision)"},
			apiEndpoint{Method: "POST", Path: "/scripts/{id}/recover", Purpose: "recompile a FROZEN or BROKEN script"},
			apiEndpoint{Method: "GET", Path: "/v1/modem", Purpose: "cached modem snapshot, refreshed every 10s"},
			apiEndpoint{Method: "POST", Path: "/v1/modem/query", Purpose: "run one read-only modem command"},
			apiEndpoint{Method: "PUT", Path: "/v1/modem/radio?preview=true", Purpose: "list the steps of a radio change without sending them"},
			apiEndpoint{Method: "PUT", Path: "/v1/modem/radio", Purpose: "apply radio settings and save them to kam.yml"},
		)
		writeJSON(w, 200, map[string]any{
			"api":         "kam",
			"api_version": 1,
			"endpoints":   endpoints,
			"docs":        "docs/APIdetails.md",
		})
	}
	mux.HandleFunc("GET /{$}", indexHandler)
	slots := make(chan struct{}, 8)
	timed := http.TimeoutHandler(mux, requestTimeout, `{"error":"request timed out"}`)
	// The radio apply is the one long exception (§7.5.1): up to ~10 s for the
	// change and up to ~5 s for recovery. The connection's write deadline is
	// extended per request so the buffered response still gets out.
	timedRadio := http.TimeoutHandler(mux, modemApplyTimeout, `{"error":"request timed out"}`)
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		defer func() {
			if recover() != nil {
				slog.Error("local API handler panicked")
				writeJSON(w, 500, map[string]string{"error": "internal error"})
			}
		}()
		if !loopbackHost(request.Host) {
			writeJSON(w, 403, map[string]string{"error": "local API serves loopback hosts only"})
			return
		}
		if request.Method == http.MethodPost || request.Method == http.MethodPut {
			if mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type")); err != nil || mediaType != "application/json" {
				writeJSON(w, 415, map[string]string{"error": "Content-Type must be application/json"})
				return
			}
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			writeJSON(w, 503, map[string]string{"error": "busy"})
			return
		}
		if isModemRadioApply(request.Method, request.URL.Path) {
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(modemApplyTimeout + 2*time.Second))
			timedRadio.ServeHTTP(w, request)
			return
		}
		timed.ServeHTTP(w, request)
	})
}

// loopbackHost reports whether the request's Host header names the local
// machine; it stops web pages from driving the API via DNS rebinding or
// cross-site forms.
func loopbackHost(hostport string) bool {
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

// requestTimeout bounds ordinary requests. modemApplyTimeout is the one
// bounded exception for PUT /v1/modem/radio (§7.5.1); a test may shorten it.
const requestTimeout = 5 * time.Second

var modemApplyTimeout = 16 * time.Second

func writeJSON(w http.ResponseWriter, code int, value any) {
	body, err := json.Marshal(value)
	if err != nil || len(body) > 8<<20 {
		http.Error(w, "response unavailable", 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

func Listen(address string, handler http.Handler) (*http.Server, error) {
	config := models.DefaultMonitorConfig()
	config.Listen = address
	if err := config.Validate(); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	server := &http.Server{Addr: listener.Addr().String(), Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			slog.Error("local API listener stopped", "error", err)
		}
	}()
	return server, nil
}
