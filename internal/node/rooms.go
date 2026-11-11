package node

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"kam.bytecoder.nl/internal/api"
	"kam.bytecoder.nl/internal/db"
	"kam.bytecoder.nl/internal/room"
)

type apiRooms struct {
	service     *room.Service
	repository  *db.RoomRepository
	persistence *db.Persistence
}

func (r apiRooms) Rooms(ctx context.Context) ([]api.Room, error) {
	snapshot, err := r.service.MonitorSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]api.Room, 0, len(snapshot))
	for _, room := range snapshot {
		stats, err := r.repository.MonitorStats(ctx, room.ID)
		if err != nil {
			return nil, err
		}
		items = append(items, api.Room{
			ID: room.ID, Name: room.Name, Enabled: room.Enabled, PublicKey: room.PublicKey,
			MaxClients: room.MaxClients, ClientsRemembered: len(room.Clients),
			LastActivityAt: room.LastActivityAt, LastPostAt: stats.LastPostAt, RetainedPosts: stats.RetainedPosts,
			Counters: api.RoomCounters{
				LoginsOK: room.Counters.LoginsOK, LoginsFailed: room.Counters.LoginsFailed, LoginsRateLimited: room.Counters.LoginsRateLimited,
				PostsAccepted: room.Counters.PostsAccepted, PostsRejected: room.Counters.PostsRejected, PushesSent: room.Counters.PushesSent,
				PushesAcked: room.Counters.PushesAcked, PushTimeouts: room.Counters.PushTimeouts,
			},
		})
	}
	return items, nil
}

func (r apiRooms) Clients(ctx context.Context, id string) ([]api.RoomClient, error) {
	snapshot, err := r.service.MonitorSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	for _, room := range snapshot {
		if room.ID != id {
			continue
		}
		clients := make([]api.RoomClient, 0, len(room.Clients))
		for _, client := range room.Clients {
			publicKey, err := hex.DecodeString(client.PublicKey)
			if err != nil {
				return nil, fmt.Errorf("decode room client key: %w", err)
			}
			label, err := r.persistence.AdvertNickname(ctx, publicKey)
			if err != nil {
				return nil, err
			}
			clients = append(clients, api.RoomClient{PublicKey: client.PublicKey, Role: roomRoleName(client.Role), LastActivityAt: client.LastActivityAt, PushState: client.PushState, Label: label})
		}
		sort.Slice(clients, func(i, j int) bool { return clients[i].PublicKey < clients[j].PublicKey })
		return clients, nil
	}
	return nil, fmt.Errorf("room %q not found", id)
}

func (r apiRooms) Posts(ctx context.Context, id string, limit int) ([]api.RoomPost, error) {
	posts, err := r.repository.MonitorPosts(ctx, id, limit)
	if err != nil {
		return nil, err
	}
	items := make([]api.RoomPost, 0, len(posts))
	for _, post := range posts {
		items = append(items, api.RoomPost{ID: post.Timestamp, CreatedAt: post.CreatedAt, AuthorKey: hex.EncodeToString(post.Author), Body: post.Text})
	}
	return items, nil
}

// RemoveClient hands the removal to the room worker and maps the room's
// unknown-client answer to the API's not-found sentinel.
func (r apiRooms) RemoveClient(ctx context.Context, roomID string, publicKey [32]byte) error {
	err := r.service.RemoveClient(ctx, roomID, publicKey)
	if errors.Is(err, room.ErrUnknownClient) {
		return fmt.Errorf("%w: %w", api.ErrRoomClientNotFound, err)
	}
	return err
}

func roomRoleName(role uint8) string {
	switch role {
	case room.RoleAdmin:
		return "admin"
	case room.RoleReadWrite:
		return "read_write"
	case room.RoleReadOnly:
		return "read_only"
	default:
		return "guest"
	}
}
