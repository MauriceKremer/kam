package api

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

// Advert is one stored advert record: the newest advert heard from one node.
// The adverts table keeps one row per public key, so a reheard node replaces
// its row; names and coordinates are what that node last advertised.
type Advert struct {
	PublicKey        string     `json:"public_key"`
	NodeHash         string     `json:"node_hash"`
	Nickname         *string    `json:"nickname"`
	IsChatNode       bool       `json:"is_chat_node"`
	IsRepeater       bool       `json:"is_repeater"`
	IsRoomServer     bool       `json:"is_room_server"`
	IsSensor         bool       `json:"is_sensor"`
	HasLocation      bool       `json:"has_location"`
	Latitude         *float64   `json:"latitude"`
	Longitude        *float64   `json:"longitude"`
	SignatureValid   bool       `json:"signature_valid"`
	HopCount         int        `json:"hop_count"`
	HopStations      []string   `json:"hop_stations"`
	TimeDriftSeconds int64      `json:"time_drift_seconds"`
	AdvertTimestamp  time.Time  `json:"advert_timestamp"`
	FirstReceivedAt  *time.Time `json:"first_received_at"`
	LastReceivedAt   time.Time  `json:"last_received_at"`
}

// AdvertPage is one bounded list of stored adverts, newest heard first.
type AdvertPage struct {
	Adverts   []Advert
	Truncated bool
}

// AdvertReader is the seam for stored adverts. A nil reader switches the route
// off, like the other optional areas.
type AdvertReader interface {
	Adverts(context.Context, int) (AdvertPage, error)
}

const (
	defaultAdvertLimit = 1000
	maxAdvertLimit     = 10000
)

func registerAdvertRoutes(mux *http.ServeMux, adverts AdvertReader) {
	if adverts == nil {
		return
	}
	mux.HandleFunc("GET /v1/adverts", func(w http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		limit := defaultAdvertLimit
		var err error
		if query.Has("limit") {
			limit, err = strconv.Atoi(query.Get("limit"))
		}
		if err != nil || limit < 1 || limit > maxAdvertLimit {
			writeJSON(w, 400, map[string]string{"error": "limit must be between 1 and 10000"})
			return
		}
		page, err := adverts.Adverts(request.Context(), limit)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "could not read adverts"})
			return
		}
		if page.Adverts == nil {
			page.Adverts = []Advert{}
		}
		writeJSON(w, 200, map[string]any{"adverts": page.Adverts, "truncated": page.Truncated})
	})
}
