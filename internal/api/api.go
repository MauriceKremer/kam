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
	"context"
	"encoding/hex"
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
	"unicode/utf8"

	"kam.bytecoder.nl/internal/models"
)

// apiEndpoint is one row of the self-describing index served at GET /.
type apiEndpoint struct {
	Method  string `json:"method"`
	Path    string `json:"path"`
	Purpose string `json:"purpose"`
}

// EventsResponse is the events-page wire shape; the events package's own Page
// carries the same fields without JSON tags, and the composition root maps
// between the two through EventsSource.
type EventsResponse struct {
	Epoch     string                `json:"epoch"`
	FirstSeq  uint64                `json:"first_seq"`
	LastSeq   uint64                `json:"last_seq"`
	NextAfter uint64                `json:"next_after"`
	Reset     bool                  `json:"reset"`
	Events    []models.MonitorEvent `json:"events"`
}

// RingStats is the ring snapshot the status handler folds into /v1/status.
type RingStats struct {
	Epoch    string
	Capacity int
	FirstSeq uint64
	LastSeq  uint64
}

// EventsSource is the read seam over the buffered event ring.
type EventsSource interface {
	Page(after uint64, epoch string, limit int) (EventsResponse, error)
	Stats() RingStats
}

type Channel struct {
	Name           string     `json:"name"`
	KeyStrategy    string     `json:"key_strategy"`
	Enabled        bool       `json:"enabled"`
	StoredMessages int64      `json:"stored_messages"`
	LastMessageAt  *time.Time `json:"last_message_at"`
}

type ChannelMessage struct {
	ID         int64     `json:"id"`
	ReceivedAt time.Time `json:"received_at"`
	Sender     *string   `json:"sender"`
	Body       *string   `json:"body"`
	HopCount   int       `json:"hop_count"`
	SNRDB      *float64  `json:"snr_db"`
	RSSIDBm    *int      `json:"rssi_dbm"`
}

type ChannelPage struct {
	Messages     []ChannelMessage `json:"messages"`
	NextBeforeID *int64           `json:"next_before_id"`
}

type ChannelReader interface {
	Channels(context.Context) ([]Channel, error)
	Messages(context.Context, string, int, int64, string) (ChannelPage, error)
}

type RoomCounters struct {
	LoginsOK          uint64 `json:"logins_ok"`
	LoginsFailed      uint64 `json:"logins_failed"`
	LoginsRateLimited uint64 `json:"logins_rate_limited"`
	PostsAccepted     uint64 `json:"posts_accepted"`
	PostsRejected     uint64 `json:"posts_rejected"`
	PushesSent        uint64 `json:"pushes_sent"`
	PushesAcked       uint64 `json:"pushes_acked"`
	PushTimeouts      uint64 `json:"push_timeouts"`
}

type Room struct {
	ID                string       `json:"id"`
	Name              string       `json:"name"`
	Enabled           bool         `json:"enabled"`
	PublicKey         string       `json:"public_key"`
	MaxClients        int          `json:"max_clients"`
	ClientsRemembered int          `json:"clients_remembered"`
	LastActivityAt    *time.Time   `json:"last_activity_at"`
	LastPostAt        *time.Time   `json:"last_post_at"`
	RetainedPosts     int64        `json:"retained_posts"`
	Counters          RoomCounters `json:"counters"`
}

type RoomClient struct {
	PublicKey      string     `json:"public_key"`
	Role           string     `json:"role"`
	LastActivityAt *time.Time `json:"last_activity_at"`
	PushState      string     `json:"push_state"`
	Label          *string    `json:"label"`
}

type RoomPost struct {
	ID        uint32    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	AuthorKey string    `json:"author_key"`
	Body      string    `json:"body"`
}

// RoomSource is the rooms seam: three reads and the removal of one remembered
// client.
type RoomSource interface {
	Rooms(context.Context) ([]Room, error)
	Clients(context.Context, string) ([]RoomClient, error)
	Posts(context.Context, string, int) ([]RoomPost, error)
	// RemoveClient forgets one client of a room. It returns an error that
	// wraps ErrRoomClientNotFound when the room has no such client.
	RemoveClient(ctx context.Context, roomID string, publicKey [32]byte) error
}

// ErrRoomClientNotFound is the RoomSource answer for a client the room does
// not remember; the handler turns it into 404.
var ErrRoomClientNotFound = errors.New("room client not found")

type RadioStatus struct {
	FrequencyHz     uint32 `json:"frequency_hz"`
	BandwidthHz     uint32 `json:"bandwidth_hz"`
	SpreadingFactor uint8  `json:"spreading_factor"`
	CodingRate      uint8  `json:"coding_rate"`
	TxPowerDBM      *uint8 `json:"tx_power_dbm"`
	Source          string `json:"source"`
}
type ModemStatus struct {
	Link            string  `json:"link"`
	DeviceName      *string `json:"device_name"`
	FirmwareVersion *uint8  `json:"firmware_version"`
}
type TXStatus struct {
	Queued       int    `json:"queued"`
	Submitted    uint64 `json:"submitted"`
	Sent         uint64 `json:"sent"`
	Completed    uint64 `json:"completed"`
	Rejected     uint64 `json:"rejected"`
	Evicted      uint64 `json:"evicted"`
	Deferred     uint64 `json:"deferred"`
	GateTimeouts uint64 `json:"gate_timeouts"`
}
type DutyStatus struct {
	ConfiguredPercent float64 `json:"configured_percent"`
	UsedMS            int64   `json:"used_ms"`
	WindowS           int     `json:"window_s"`
}
type QueueStatus struct {
	Depth   int    `json:"depth"`
	Dropped uint64 `json:"dropped"`
}
type RoomStatus struct {
	Enabled bool `json:"enabled"`
	Count   int  `json:"count"`
}
type AutomationStatus struct {
	Enabled        bool    `json:"enabled"`
	ScriptsEnabled *int    `json:"scripts_enabled"`
	QueueDepth     *int    `json:"queue_depth"`
	EventsDropped  *uint64 `json:"events_dropped"`
}
type RingStatus struct {
	Capacity int    `json:"capacity"`
	FirstSeq uint64 `json:"first_seq"`
	LastSeq  uint64 `json:"last_seq"`
}
type Status struct {
	APIVersion  int                    `json:"api_version"`
	Epoch       string                 `json:"epoch"`
	Now         time.Time              `json:"now"`
	StartedAt   time.Time              `json:"started_at"`
	NodeName    string                 `json:"node_name"`
	Modem       ModemStatus            `json:"modem"`
	Radio       RadioStatus            `json:"radio"`
	DutyCycle   DutyStatus             `json:"duty_cycle"`
	TX          TXStatus               `json:"tx"`
	Persistence map[string]QueueStatus `json:"persistence"`
	Rooms       RoomStatus             `json:"rooms"`
	Automation  AutomationStatus       `json:"automation"`
	EventRing   RingStatus             `json:"event_ring"`
	System      *SystemStatus          `json:"system,omitempty"`
}

// SystemStatus is the opt-in host snapshot served when a status request asks
// for it via ?system=true. Collection and its throttling live in
// internal/systemstats; the system seam hands in the one snapshot function.
type SystemStatus struct {
	CollectedAt time.Time     `json:"collected_at"`
	Memory      *SystemMemory `json:"memory"`
	CPU         *SystemCPU    `json:"cpu"`
	Disk        *SystemDisk   `json:"disk"`
	Database    *SystemDB     `json:"database"`
}
type SystemMemory struct {
	TotalBytes     uint64  `json:"total_bytes"`
	AvailableBytes uint64  `json:"available_bytes"`
	UsedBytes      uint64  `json:"used_bytes"`
	UsedPercent    float64 `json:"used_percent"`
}
type SystemCPU struct {
	Model   *string  `json:"model"`
	Cores   int      `json:"cores"`
	Load1m  *float64 `json:"load_1m"`
	Load5m  *float64 `json:"load_5m"`
	Load15m *float64 `json:"load_15m"`
}
type SystemDisk struct {
	Path           string  `json:"path"`
	TotalBytes     uint64  `json:"total_bytes"`
	AvailableBytes uint64  `json:"available_bytes"`
	UsedBytes      uint64  `json:"used_bytes"`
	UsedPercent    float64 `json:"used_percent"`
}
type SystemDB struct {
	Path         string  `json:"path"`
	SizeBytes    uint64  `json:"size_bytes"`
	WalSizeBytes *uint64 `json:"wal_size_bytes"`
}

func NewHandler(ring EventsSource, status func() Status) http.Handler {
	return NewHandlerWithResources(ring, status, nil, nil, nil, nil, nil)
}

func NewHandlerWithChannels(ring EventsSource, status func() Status, channels ChannelReader) http.Handler {
	return NewHandlerWithResources(ring, status, nil, channels, nil, nil, nil)
}

// NewHandlerWithResources composes the whole listener. system hands in the
// throttled host-snapshot collector; nil hides the system section even when a
// request asks for it.
func NewHandlerWithResources(ring EventsSource, status func() Status, scripts Scripts, channels ChannelReader, rooms RoomSource, modem ModemSource, system func() *SystemStatus) http.Handler {
	mux := http.NewServeMux()
	registerScriptRoutes(mux, scripts)
	registerModemRoutes(mux, modem)
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		after, err := strconv.ParseUint(query.Get("after"), 10, 64)
		if query.Get("after") == "" {
			after, err = 0, nil
		}
		if err != nil || (after > 0 && query.Get("epoch") == "") {
			writeJSON(w, 400, map[string]string{"error": "after must be non-negative and a nonzero cursor requires epoch"})
			return
		}
		limit := 1000
		if query.Has("limit") {
			limit, err = strconv.Atoi(query.Get("limit"))
		}
		if err != nil || limit < 1 || limit > 2000 {
			writeJSON(w, 400, map[string]string{"error": "limit must be between 1 and 2000"})
			return
		}
		page, err := ring.Page(after, query.Get("epoch"), limit)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, page)
	})
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		value := status()
		stats := ring.Stats()
		value.APIVersion, value.Epoch, value.Now = 1, stats.Epoch, time.Now().UTC().Truncate(time.Millisecond)
		value.EventRing = RingStatus{Capacity: stats.Capacity, FirstSeq: stats.FirstSeq, LastSeq: stats.LastSeq}
		if query.Has("system") {
			switch query.Get("system") {
			case "true", "1":
				if system != nil {
					value.System = system()
				}
			default:
				writeJSON(w, 400, map[string]string{"error": "system must be 1 or true"})
				return
			}
		}
		writeJSON(w, 200, value)
	})
	if channels != nil {
		mux.HandleFunc("GET /v1/channels", func(w http.ResponseWriter, request *http.Request) {
			items, err := channels.Channels(request.Context())
			if err != nil {
				writeJSON(w, 500, map[string]string{"error": "could not read channels"})
				return
			}
			if items == nil {
				items = []Channel{}
			}
			writeJSON(w, 200, map[string]any{"channels": items})
		})
		mux.HandleFunc("GET /v1/channels/{name}/messages", func(w http.ResponseWriter, request *http.Request) {
			name := request.PathValue("name")
			items, err := channels.Channels(request.Context())
			if err != nil {
				writeJSON(w, 500, map[string]string{"error": "could not read channels"})
				return
			}
			known := false
			for _, item := range items {
				if item.Name == name {
					known = true
					break
				}
			}
			if !known {
				writeJSON(w, 404, map[string]string{"error": "channel not found"})
				return
			}
			query := request.URL.Query()
			limit := 50
			if query.Has("limit") {
				limit, err = strconv.Atoi(query.Get("limit"))
			}
			if err != nil || limit < 1 || limit > 200 {
				writeJSON(w, 400, map[string]string{"error": "limit must be between 1 and 200"})
				return
			}
			var beforeID int64
			if query.Has("before_id") {
				beforeID, err = strconv.ParseInt(query.Get("before_id"), 10, 64)
			}
			if err != nil || beforeID < 0 || (query.Has("before_id") && beforeID == 0) {
				writeJSON(w, 400, map[string]string{"error": "before_id must be a positive integer"})
				return
			}
			search := query.Get("q")
			if !utf8.ValidString(search) || len(search) > 200 {
				writeJSON(w, 400, map[string]string{"error": "q must be at most 200 UTF-8 bytes"})
				return
			}
			page, err := channels.Messages(request.Context(), name, limit, beforeID, search)
			if err != nil {
				writeJSON(w, 500, map[string]string{"error": "could not read channel messages"})
				return
			}
			if page.Messages == nil {
				page.Messages = []ChannelMessage{}
			}
			writeJSON(w, 200, map[string]any{"channel": name, "messages": page.Messages, "next_before_id": page.NextBeforeID})
		})
	}
	mux.HandleFunc("GET /v1/rooms", func(w http.ResponseWriter, request *http.Request) {
		if rooms == nil {
			writeJSON(w, 503, map[string]string{"error": "rooms are disabled"})
			return
		}
		items, err := rooms.Rooms(request.Context())
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "could not read rooms"})
			return
		}
		if items == nil {
			items = []Room{}
		}
		writeJSON(w, 200, map[string]any{"rooms": items})
	})
	mux.HandleFunc("GET /v1/rooms/{id}/clients", func(w http.ResponseWriter, request *http.Request) {
		if rooms == nil {
			writeJSON(w, 503, map[string]string{"error": "rooms are disabled"})
			return
		}
		id := request.PathValue("id")
		known, err := roomExists(request.Context(), rooms, id)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "could not read rooms"})
			return
		}
		if !known {
			writeJSON(w, 404, map[string]string{"error": "room not found"})
			return
		}
		clients, err := rooms.Clients(request.Context(), id)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "could not read room clients"})
			return
		}
		if clients == nil {
			clients = []RoomClient{}
		}
		writeJSON(w, 200, map[string]any{"room": id, "clients": clients})
	})
	mux.HandleFunc("GET /v1/rooms/{id}/posts", func(w http.ResponseWriter, request *http.Request) {
		if rooms == nil {
			writeJSON(w, 503, map[string]string{"error": "rooms are disabled"})
			return
		}
		id := request.PathValue("id")
		known, err := roomExists(request.Context(), rooms, id)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "could not read rooms"})
			return
		}
		if !known {
			writeJSON(w, 404, map[string]string{"error": "room not found"})
			return
		}
		limit := 20
		var parseErr error
		if request.URL.Query().Has("limit") {
			limit, parseErr = strconv.Atoi(request.URL.Query().Get("limit"))
		}
		if parseErr != nil || limit < 1 || limit > 200 {
			writeJSON(w, 400, map[string]string{"error": "limit must be between 1 and 200"})
			return
		}
		posts, err := rooms.Posts(request.Context(), id, limit)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "could not read room posts"})
			return
		}
		if posts == nil {
			posts = []RoomPost{}
		}
		writeJSON(w, 200, map[string]any{"room": id, "posts": posts})
	})
	mux.HandleFunc("DELETE /v1/rooms/{id}/clients/{key}", func(w http.ResponseWriter, request *http.Request) {
		if rooms == nil {
			writeJSON(w, 503, map[string]string{"error": "rooms are disabled"})
			return
		}
		id := request.PathValue("id")
		known, err := roomExists(request.Context(), rooms, id)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "could not read rooms"})
			return
		}
		if !known {
			writeJSON(w, 404, map[string]string{"error": "room not found"})
			return
		}
		key, ok := parsePublicKey(request.PathValue("key"))
		if !ok {
			writeJSON(w, 400, map[string]string{"error": "key must be a 64-character hex public key"})
			return
		}
		err = rooms.RemoveClient(request.Context(), id, key)
		if errors.Is(err, ErrRoomClientNotFound) {
			writeJSON(w, 404, map[string]string{"error": "client not found"})
			return
		}
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "could not remove room client"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
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

// parsePublicKey decodes a path key of exactly 64 hex characters, in either
// letter case.
func parsePublicKey(text string) ([32]byte, bool) {
	var key [32]byte
	if len(text) != hex.EncodedLen(len(key)) {
		return key, false
	}
	_, err := hex.Decode(key[:], []byte(text))
	return key, err == nil
}

func roomExists(ctx context.Context, source RoomSource, id string) (bool, error) {
	rooms, err := source.Rooms(ctx)
	if err != nil {
		return false, err
	}
	for _, room := range rooms {
		if room.ID == id {
			return true, nil
		}
	}
	return false, nil
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
