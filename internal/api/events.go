package api

import (
	"net/http"
	"strconv"

	"kam.bytecoder.nl/internal/models"
)

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

func registerEventRoutes(mux *http.ServeMux, ring EventsSource) {
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
}
