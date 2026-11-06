package room

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"kam.bytecoder.nl/internal/db"
)

// A system post is stored like a client post: it lands in the backlog, reaches
// logged-in clients through the normal push path, and names the room itself as
// its author.
func TestSystemPostIsStoredAndPushedToClients(t *testing.T) {
	h := newHarness(t, nil)
	reader := h.loggedIn(1, "guest-pass")

	const text = "weather 4.9 C"
	if err := h.svc.addSystemPost("lobby", text, h.now); err != nil {
		t.Fatalf("addSystemPost: %v", err)
	}
	posts, err := h.repo.LatestPosts(testContext(), "lobby", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 1 || posts[0].Text != text {
		t.Fatalf("stored posts = %+v", posts)
	}
	if !bytes.Equal(posts[0].Author, h.room.pub[:]) {
		t.Fatalf("author = %x, want the room key %x", posts[0].Author, h.room.pub[:])
	}
	if got := h.svc.Stats().PostsAccepted; got != 1 {
		t.Fatalf("posts accepted = %d", got)
	}

	var push sentPacket
	for i := 0; i < 200 && push.Req.Raw == nil; i++ {
		h.advance(100 * time.Millisecond)
		if got := h.tx.take(); len(got) > 0 {
			push = got[0]
		}
	}
	if push.Req.Raw == nil {
		t.Fatal("the system post was never pushed")
	}
	plain := reader.open(push)
	got := string(bytes.TrimRight(plain[9:], "\x00"))
	if !bytes.Equal(plain[5:9], h.room.pub[:4]) || got != text {
		t.Fatalf("push plaintext %x, want the room as author and %q", plain, text)
	}
}

func TestSystemPostTextIsTruncatedLikeAClientPost(t *testing.T) {
	h := newHarness(t, nil)
	long := strings.Repeat("a", maxPostTextLen+20)
	if err := h.svc.addSystemPost("lobby", long, h.now); err != nil {
		t.Fatalf("addSystemPost: %v", err)
	}
	posts, err := h.repo.LatestPosts(testContext(), "lobby", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 1 || len(posts[0].Text) != maxPostTextLen {
		t.Fatalf("stored post length = %d, want %d", len(posts[0].Text), maxPostTextLen)
	}
}

func TestSystemPostToAnUnknownRoomIsAnError(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.svc.addSystemPost("nowhere", "text", h.now); err == nil {
		t.Fatal("an unknown room was accepted")
	}
	if posts, _ := h.repo.LatestPosts(testContext(), "lobby", 10); len(posts) != 0 {
		t.Fatalf("stored %d posts", len(posts))
	}
}

// The worker owns room state, so AddSystemPost hands the request over instead
// of touching rooms from the caller's goroutine.
func TestAddSystemPostRunsOnTheWorker(t *testing.T) {
	h := newHarness(t, nil)
	h.svc.Start()
	defer h.svc.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for i := 0; i < 5; i++ {
		if err := h.svc.AddSystemPost(ctx, "lobby", "tick"); err != nil {
			t.Fatalf("AddSystemPost: %v", err)
		}
	}
	posts, err := h.repo.LatestPosts(testContext(), "lobby", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 5 {
		t.Fatalf("stored %d posts, want 5", len(posts))
	}
	// A room that is not configured is reported back to the caller.
	if err := h.svc.AddSystemPost(ctx, "nowhere", "text"); err == nil {
		t.Fatal("an unknown room was accepted")
	}
}

func TestAddSystemPostFailsWhenTheServiceIsStopped(t *testing.T) {
	h := newHarness(t, nil)
	h.svc.Start()
	h.svc.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := h.svc.AddSystemPost(ctx, "lobby", "text"); err == nil {
		t.Fatal("a stopped service accepted a post")
	}
}

func TestAddSystemPostReportsAFullQueue(t *testing.T) {
	h := newHarness(t, nil)
	// The worker never runs, so nothing drains the queue. A canceled context
	// keeps each call immediate; only the full queue can report an error at
	// the end of the loop.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var lastErr error
	for i := 0; i < systemPostQueue+1; i++ {
		lastErr = h.svc.AddSystemPost(ctx, "lobby", "text")
	}
	if lastErr == nil || !strings.Contains(lastErr.Error(), "full") {
		t.Fatalf("error = %v, want a full-queue error", lastErr)
	}
}

// A room-authored post passes a nil client key, so no client timestamp moves.
func TestSystemPostDoesNotAdvanceAClientTimestamp(t *testing.T) {
	h := newHarness(t, nil)
	client := h.loggedIn(1, "guest-pass")
	before := h.room.byKey[client.pub()].lastTimestamp
	if err := h.svc.addSystemPost("lobby", "text", h.now); err != nil {
		t.Fatalf("addSystemPost: %v", err)
	}
	if after := h.room.byKey[client.pub()].lastTimestamp; after != before {
		t.Fatalf("client timestamp moved from %d to %d", before, after)
	}
}

// A store that keeps claiming the timestamp was taken fails the post after its
// retries instead of looping.
func TestSystemPostRetriesOnTimestampCollision(t *testing.T) {
	h := newHarnessWith(t, harnessOptions{store: func(store Store) Store {
		return &alwaysTakenStore{Store: store}
	}})
	if err := h.svc.addSystemPost("lobby", "text", h.now); !errors.Is(err, db.ErrRoomPostTimestampTaken) {
		t.Fatalf("error = %v, want the timestamp error", err)
	}
}

func TestSystemPostSucceedsOnTimestampRetry(t *testing.T) {
	h := newHarnessWith(t, harnessOptions{store: func(store Store) Store {
		return &onceTakenStore{Store: store}
	}})
	if err := h.svc.addSystemPost("lobby", "text", h.now); err != nil {
		t.Fatalf("addSystemPost: %v", err)
	}
	posts, err := h.repo.LatestPosts(testContext(), "lobby", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 1 || posts[0].Timestamp != uint32(h.now.Unix())+1 {
		t.Fatalf("posts = %+v", posts)
	}
}

type alwaysTakenStore struct{ Store }

func (s *alwaysTakenStore) AppendPost(context.Context, string, db.RoomPost, []byte, uint32) error {
	return db.ErrRoomPostTimestampTaken
}

type onceTakenStore struct {
	Store
	failed bool
}

func (s *onceTakenStore) AppendPost(ctx context.Context, roomID string, post db.RoomPost, key []byte, ts uint32) error {
	if !s.failed {
		s.failed = true
		return db.ErrRoomPostTimestampTaken
	}
	return s.Store.AppendPost(ctx, roomID, post, key, ts)
}
