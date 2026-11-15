package api

import (
	"context"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"
)

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

// registerChannelRoutes adds the channel routes only when a reader was handed
// in; without one they answer 404.
func registerChannelRoutes(mux *http.ServeMux, channels ChannelReader) {
	if channels == nil {
		return
	}
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
