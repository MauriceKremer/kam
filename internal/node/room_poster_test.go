package node

import (
	"context"
	"crypto/rand"
	"path/filepath"
	"testing"
	"time"

	"kam.bytecoder.nl/internal/db"
	"kam.bytecoder.nl/internal/kiss"
	"kam.bytecoder.nl/internal/mesh"
	"kam.bytecoder.nl/internal/models"
	"kam.bytecoder.nl/internal/room"
)

// discardingTransmitter accepts everything and forgets it.
type discardingTransmitter struct{}

func (discardingTransmitter) Submit(kiss.TxRequest) error { return nil }
func (discardingTransmitter) Cancel(string) int           { return 0 }

// A script posting through the automation seam reaches the room's backlog, and
// an unknown room comes back as an error rather than a panic or a silent drop.
func TestRoomPosterAdapterStoresIntoTheRoom(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "kam.db")
	repository, err := db.OpenRoomRepository(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	identity, err := mesh.GenerateIdentity(rand.Reader, func(byte) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	service, err := room.NewService(room.Options{
		Rooms:       []room.Spec{{Config: testRoomConfig(), Identity: identity}},
		Mesh:        models.DefaultMeshConfig(),
		Store:       repository,
		Transmitter: discardingTransmitter{},
	})
	if err != nil {
		t.Fatal(err)
	}
	service.Start()
	t.Cleanup(service.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	poster := roomPoster{service: service}
	if err := poster.Post(ctx, "lobby", "weather 4.9 C"); err != nil {
		t.Fatalf("Post: %v", err)
	}
	posts, err := repository.LatestPosts(ctx, "lobby", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 1 || posts[0].Text != "weather 4.9 C" {
		t.Fatalf("stored posts = %+v", posts)
	}
	if err := poster.Post(ctx, "nowhere", "text"); err == nil {
		t.Fatal("an unknown room was accepted")
	}
}
