package api

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"time"
)

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

func registerRoomRoutes(mux *http.ServeMux, rooms RoomSource) {
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
