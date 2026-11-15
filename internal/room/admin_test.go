package room

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"kam.bytecoder.nl/internal/kiss"
	"kam.bytecoder.nl/internal/mesh"
)

// The management calls from the local API (RS-OPS-2): forget a client, delete
// a post, send an advert now. Each one runs on the service worker.

func adminContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestRemoveClientForgetsTheSession(t *testing.T) {
	h := newHarness(t, nil)
	h.svc.Start()
	defer h.svc.Stop()
	client := h.loggedIn(1, "guest-pass")
	ctx := adminContext(t)

	if err := h.svc.RemoveClient(ctx, "lobby", hex.EncodeToString(client.pubSlice())); err != nil {
		t.Fatalf("RemoveClient: %v", err)
	}
	if h.room.byKey[client.pub()] != nil || len(h.room.clients) != 0 || h.svc.Stats().Clients != 0 {
		t.Fatalf("client still remembered: %+v", h.room.clients)
	}
	stored, err := h.repo.LoadClients(testContext(), "lobby")
	if err != nil || len(stored) != 0 {
		t.Fatalf("stored clients %d, err %v", len(stored), err)
	}
	// Removal is not a ban: the same password logs the client back in.
	h.login(client, "guest-pass", 0, floodVia())
	if h.room.byKey[client.pub()] == nil {
		t.Fatal("client could not log in again after removal")
	}
	// A key the room does not know, a key that is not a key, an unknown room.
	if err := h.svc.RemoveClient(ctx, "lobby", hex.EncodeToString(bytes.Repeat([]byte{0xAA}, 32))); !errors.Is(err, ErrClientNotFound) {
		t.Fatalf("unknown client: %v", err)
	}
	if err := h.svc.RemoveClient(ctx, "lobby", "abcd"); err == nil || errors.Is(err, ErrClientNotFound) {
		t.Fatalf("malformed key: %v", err)
	}
	if err := h.svc.RemoveClient(ctx, "nowhere", hex.EncodeToString(client.pubSlice())); !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("unknown room: %v", err)
	}
}

// API-RM-7: a persistence failure leaves the session intact and is reported,
// so batch writes cannot resurrect a removed client later.
func TestRemoveClientKeepsTheSessionWhenTheDeleteFails(t *testing.T) {
	store := &flakyStore{deleteErr: errors.New("disk full")}
	h := newHarnessWith(t, harnessOptions{store: func(s Store) Store {
		store.Store = s
		return store
	}})
	h.svc.Start()
	defer h.svc.Stop()
	client := h.loggedIn(1, "guest-pass")
	if err := h.svc.RemoveClient(adminContext(t), "lobby", hex.EncodeToString(client.pubSlice())); err == nil {
		t.Fatal("a failed delete was reported as success")
	}
	if h.room.byKey[client.pub()] == nil {
		t.Fatal("the session was dropped even though the record survived")
	}
}

func TestDeletePostRemovesItFromStoreAndBacklog(t *testing.T) {
	h := newHarness(t, nil)
	author := h.loggedIn(1, "guest-pass")
	pkt, _ := author.text("gone tomorrow", floodVia())
	h.svc.process(pkt, h.now) // no worker yet, so the room is driven directly
	h.tx.take()
	if len(h.room.posts) != 1 || h.postCount() != 1 {
		t.Fatalf("cached %d posts, stored %d", len(h.room.posts), h.postCount())
	}
	postID := h.room.posts[0].Timestamp
	h.svc.Start()
	defer h.svc.Stop()
	ctx := adminContext(t)

	if err := h.svc.DeletePost(ctx, "lobby", postID); err != nil {
		t.Fatalf("DeletePost: %v", err)
	}
	if len(h.room.posts) != 0 || h.postCount() != 0 {
		t.Fatalf("cached %d posts, stored %d after delete", len(h.room.posts), h.postCount())
	}
	if err := h.svc.DeletePost(ctx, "lobby", postID); !errors.Is(err, ErrPostNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if err := h.svc.DeletePost(ctx, "nowhere", postID); !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("unknown room: %v", err)
	}
}

func TestForceAdvertQueuesAFloodAdvert(t *testing.T) {
	h := newHarness(t, nil)
	h.svc.Start()
	defer h.svc.Stop()
	ctx := adminContext(t)
	if err := h.svc.ForceAdvert(ctx, "lobby"); err != nil {
		t.Fatalf("ForceAdvert: %v", err)
	}
	sp := h.tx.only(t)
	if sp.Pkt.PayloadType() != mesh.PayloadAdvert || !sp.Pkt.IsFlood() || sp.Req.Priority != kiss.TxPriorityAdvert {
		t.Fatalf("advert %+v", sp)
	}
	if err := h.svc.ForceAdvert(ctx, "nowhere"); !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("unknown room: %v", err)
	}
}
