package db

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func openRoomTestRepository(t *testing.T, path string) *RoomRepository {
	t.Helper()
	repository, err := OpenRoomRepository(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	return repository
}

func roomKey(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func TestRoomSchemaCoexistsWithLegacyDatabaseAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kam.db")
	persistence, err := NewPersistence(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := persistence.Close(); err != nil {
		t.Fatal(err)
	}
	repository := openRoomTestRepository(t, path)
	if columns, err := tableColumns(repository.db, "adverts"); err != nil || !columns["hop_count"] {
		t.Fatalf("legacy adverts table damaged: %v, error %v", columns, err)
	}
	for i := 0; i < 3; i++ {
		if err := EnsureRoomSchema(repository.db); err != nil {
			t.Fatalf("schema creation is not idempotent: %v", err)
		}
	}
	var versions int
	if err := repository.db.QueryRow(`SELECT COUNT(*) FROM room_schema_version`).Scan(&versions); err != nil || versions != len(roomMigrations) {
		t.Fatalf("recorded versions: %d (error %v), want %d", versions, err, len(roomMigrations))
	}

	if _, err := repository.db.Exec(`INSERT INTO room_schema_version (version) VALUES (99)`); err != nil {
		t.Fatal(err)
	}
	if err := EnsureRoomSchema(repository.db); err == nil {
		t.Fatal("a database from a newer build was accepted")
	}
}

func TestRoomRepositoryUsesFullSynchronousMode(t *testing.T) {
	repository := openRoomTestRepository(t, filepath.Join(t.TempDir(), "kam.db"))
	var mode int
	if err := repository.db.QueryRow(`PRAGMA synchronous`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != 2 {
		t.Fatalf("synchronous = %d, want 2 (FULL): an acknowledged post must survive power loss", mode)
	}
}

func TestRegisterRoomDetectsChangedIdentity(t *testing.T) {
	repository := openRoomTestRepository(t, filepath.Join(t.TempDir(), "kam.db"))
	ctx := context.Background()
	if previous, err := repository.RegisterRoom(ctx, "lobby", roomKey(1)); err != nil || previous != nil {
		t.Fatalf("first registration: %x, %v", previous, err)
	}
	if previous, err := repository.RegisterRoom(ctx, "lobby", roomKey(1)); err != nil || previous != nil {
		t.Fatalf("same key: %x, %v", previous, err)
	}
	if previous, err := repository.RegisterRoom(ctx, "lobby", roomKey(2)); err != nil || !bytes.Equal(previous, roomKey(1)) {
		t.Fatalf("changed key: previous %x, error %v", previous, err)
	}
	if previous, err := repository.RegisterRoom(ctx, "lobby", roomKey(2)); err != nil || previous != nil {
		t.Fatalf("after update: %x, %v", previous, err)
	}
	if previous, err := repository.RegisterRoom(ctx, "other", roomKey(2)); err != nil || previous != nil {
		t.Fatalf("second room: %x, %v", previous, err)
	}
}

func TestRoomPostsAreOrderedIsolatedAndDurable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kam.db")
	repository := openRoomTestRepository(t, path)
	ctx := context.Background()

	for ts := uint32(100); ts < 110; ts++ {
		post := RoomPost{Timestamp: ts, Author: roomKey(byte(ts)), Text: "post " + strconv.Itoa(int(ts))}
		if err := repository.AppendPost(ctx, "lobby", post, nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.AppendPost(ctx, "other", RoomPost{Timestamp: 105, Author: roomKey(1), Text: "elsewhere"}, nil, 0); err != nil {
		t.Fatalf("the same timestamp in another room must be accepted: %v", err)
	}
	if err := repository.AppendPost(ctx, "lobby", RoomPost{Timestamp: 105, Author: roomKey(1), Text: "again"}, nil, 0); !errors.Is(err, ErrRoomPostTimestampTaken) {
		t.Fatalf("duplicate timestamp: got %v", err)
	}

	latest, err := repository.LatestPosts(ctx, "lobby", 3)
	if err != nil || len(latest) != 3 || latest[0].Timestamp != 107 || latest[1].Timestamp != 108 || latest[2].Timestamp != 109 {
		t.Fatalf("latest three, oldest first: %+v, error %v", latest, err)
	}
	if latest[2].Text != "post 109" || !bytes.Equal(latest[2].Author, roomKey(109)) {
		t.Fatalf("post content: %+v", latest[2])
	}
	if latest[2].CreatedAt.IsZero() {
		t.Fatal("post creation time was not loaded")
	}
	if all, _ := repository.LatestPosts(ctx, "lobby", 100); len(all) != 10 {
		t.Fatalf("got %d posts, want 10 (the rejected duplicate must not exist)", len(all))
	}
	if count, err := repository.CountPosts(ctx, "other"); err != nil || count != 1 {
		t.Fatalf("other room: %d, %v", count, err)
	}

	// An acknowledged post must survive a restart.
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openRoomTestRepository(t, path)
	if posts, err := reopened.LatestPosts(ctx, "lobby", 100); err != nil || len(posts) != 10 {
		t.Fatalf("after reopening: %d posts, error %v", len(posts), err)
	}
}

func TestRoomMonitorQueriesReturnStatsAndNewestPostsFirst(t *testing.T) {
	repository := openRoomTestRepository(t, filepath.Join(t.TempDir(), "kam.db"))
	ctx := context.Background()
	for timestamp := uint32(1); timestamp <= 3; timestamp++ {
		if err := repository.AppendPost(ctx, "lobby", RoomPost{Timestamp: timestamp, Author: roomKey(1), Text: "post"}, nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := repository.MonitorStats(ctx, "lobby")
	if err != nil || stats.RetainedPosts != 3 || stats.LastPostAt == nil {
		t.Fatalf("room stats = %+v, %v", stats, err)
	}
	posts, err := repository.MonitorPosts(ctx, "lobby", 2)
	if err != nil || len(posts) != 2 || posts[0].Timestamp != 3 || posts[1].Timestamp != 2 || posts[0].CreatedAt.IsZero() {
		t.Fatalf("monitor posts = %+v, %v", posts, err)
	}
}

func TestAdvertNicknameUsesTheNewestRecord(t *testing.T) {
	persistence, err := NewPersistence(filepath.Join(t.TempDir(), "kam.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer persistence.Close()
	key := roomKey(7)
	advert := func(nickname string) AdvertRecord {
		return AdvertRecord{PublicKey: key, Signature: []byte{0}, RawPayload: []byte{0}, Nickname: nickname}
	}
	if err := InsertAdvertsBatch(persistence.db, []AdvertRecord{advert("Old name"), advert("Newest name")}); err != nil {
		t.Fatal(err)
	}
	label, err := persistence.AdvertNickname(context.Background(), key)
	if err != nil || label == nil || *label != "Newest name" {
		t.Fatalf("latest nickname = %v, %v", label, err)
	}
	if err := InsertAdvertsBatch(persistence.db, []AdvertRecord{advert("")}); err != nil {
		t.Fatal(err)
	}
	label, err = persistence.AdvertNickname(context.Background(), key)
	if err != nil || label != nil {
		t.Fatalf("empty newest nickname should be null: %v, %v", label, err)
	}
}

func TestRoomPostPreservesUnicodeAndEmptyAuthorSafety(t *testing.T) {
	repository := openRoomTestRepository(t, filepath.Join(t.TempDir(), "kam.db"))
	ctx := context.Background()
	text := "héllo 日本語 \"quoted\" 'single'; DROP TABLE room_posts;--"
	if err := repository.AppendPost(ctx, "lobby", RoomPost{Timestamp: 1, Author: roomKey(7), Text: text}, nil, 0); err != nil {
		t.Fatal(err)
	}
	posts, err := repository.LatestPosts(ctx, "lobby", 10)
	if err != nil || len(posts) != 1 || posts[0].Text != text {
		t.Fatalf("round trip: %+v, %v", posts, err)
	}
}

func TestAppendPostUpdatesClientTimestampInTheSameTransaction(t *testing.T) {
	repository := openRoomTestRepository(t, filepath.Join(t.TempDir(), "kam.db"))
	ctx := context.Background()
	lastHeard := time.Unix(1234, 0).UTC()
	client := RoomClient{PublicKey: roomKey(9), Role: 2, SyncSince: 5, LastTimestamp: 10, OutPathLen: 0xFF, LastHeardAt: &lastHeard}
	if err := repository.SaveClients(ctx, "lobby", []RoomClient{client}); err != nil {
		t.Fatal(err)
	}
	if err := repository.AppendPost(ctx, "lobby", RoomPost{Timestamp: 50, Author: client.PublicKey, Text: "hi"}, client.PublicKey, 42); err != nil {
		t.Fatal(err)
	}
	clients, err := repository.LoadClients(ctx, "lobby")
	if err != nil || len(clients) != 1 || clients[0].LastTimestamp != 42 || clients[0].SyncSince != 5 || clients[0].LastHeardAt == nil || !clients[0].LastHeardAt.Equal(lastHeard) {
		t.Fatalf("clients: %+v, %v", clients, err)
	}

	// A rejected post must not move the client timestamp.
	if err := repository.AppendPost(ctx, "lobby", RoomPost{Timestamp: 50, Author: client.PublicKey, Text: "dup"}, client.PublicKey, 99); !errors.Is(err, ErrRoomPostTimestampTaken) {
		t.Fatalf("duplicate: %v", err)
	}
	if clients, _ := repository.LoadClients(ctx, "lobby"); clients[0].LastTimestamp != 42 {
		t.Fatalf("rolled-back post changed the client timestamp: %+v", clients[0])
	}

	// A client without a stored row is not an error.
	if err := repository.AppendPost(ctx, "lobby", RoomPost{Timestamp: 51, Author: roomKey(1), Text: "x"}, roomKey(1), 3); err != nil {
		t.Fatalf("unknown client: %v", err)
	}
}

func TestPrunePostsByCountAndAge(t *testing.T) {
	repository := openRoomTestRepository(t, filepath.Join(t.TempDir(), "kam.db"))
	ctx := context.Background()
	for ts := uint32(1); ts <= 20; ts++ {
		if err := repository.AppendPost(ctx, "lobby", RoomPost{Timestamp: ts, Author: roomKey(1), Text: "p"}, nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.AppendPost(ctx, "other", RoomPost{Timestamp: 1, Author: roomKey(1), Text: "keep"}, nil, 0); err != nil {
		t.Fatal(err)
	}

	if n, err := repository.PrunePosts(ctx, "lobby", 0, 0); err != nil || n != 0 {
		t.Fatalf("no limits: deleted %d, %v", n, err)
	}
	if n, err := repository.PrunePosts(ctx, "lobby", 0, 6); err != nil || n != 5 {
		t.Fatalf("age: deleted %d, %v", n, err)
	}
	if n, err := repository.PrunePosts(ctx, "lobby", 10, 0); err != nil || n != 5 {
		t.Fatalf("count: deleted %d, %v", n, err)
	}
	posts, _ := repository.LatestPosts(ctx, "lobby", 100)
	if len(posts) != 10 || posts[0].Timestamp != 11 || posts[9].Timestamp != 20 {
		t.Fatalf("remaining: %+v", posts)
	}
	if n, err := repository.PrunePosts(ctx, "lobby", 50, 0); err != nil || n != 0 {
		t.Fatalf("keeping more than exist: deleted %d, %v", n, err)
	}
	if n, err := repository.PrunePosts(ctx, "lobby", 3, 15); err != nil || n != 4+3 {
		t.Fatalf("combined: deleted %d, %v", n, err)
	}
	if count, _ := repository.CountPosts(ctx, "other"); count != 1 {
		t.Fatalf("pruning touched another room: %d", count)
	}
}

func TestRoomClientsRoundTripUpsertAndDelete(t *testing.T) {
	repository := openRoomTestRepository(t, filepath.Join(t.TempDir(), "kam.db"))
	ctx := context.Background()
	routed := RoomClient{PublicKey: roomKey(1), Role: 3, SyncSince: 100, LastTimestamp: 200, OutPathLen: 0x42, OutPath: []byte{1, 2, 3, 4}}
	unrouted := RoomClient{PublicKey: roomKey(2), Role: 2, SyncSince: 1, LastTimestamp: 2, OutPathLen: 0xFF}
	if err := repository.SaveClients(ctx, "lobby", []RoomClient{routed, unrouted}); err != nil {
		t.Fatal(err)
	}
	if err := repository.SaveClients(ctx, "lobby", nil); err != nil {
		t.Fatalf("empty save: %v", err)
	}
	other := RoomClient{PublicKey: roomKey(1), Role: 0, OutPathLen: 0xFF}
	if err := repository.SaveClients(ctx, "other", []RoomClient{other}); err != nil {
		t.Fatal(err)
	}

	clients, err := repository.LoadClients(ctx, "lobby")
	if err != nil || len(clients) != 2 {
		t.Fatalf("clients: %+v, %v", clients, err)
	}
	byKey := map[byte]RoomClient{}
	for _, c := range clients {
		byKey[c.PublicKey[0]] = c
	}
	got := byKey[1]
	if got.Role != 3 || got.SyncSince != 100 || got.LastTimestamp != 200 || got.OutPathLen != 0x42 || !bytes.Equal(got.OutPath, routed.OutPath) {
		t.Fatalf("routed client: %+v", got)
	}
	if got := byKey[2]; got.OutPathLen != 0xFF || len(got.OutPath) != 0 {
		t.Fatalf("unrouted client: %+v", got)
	}

	routed.SyncSince, routed.OutPathLen, routed.OutPath = 150, 0xFF, nil
	if err := repository.SaveClients(ctx, "lobby", []RoomClient{routed}); err != nil {
		t.Fatal(err)
	}
	clients, _ = repository.LoadClients(ctx, "lobby")
	if len(clients) != 2 {
		t.Fatalf("upsert created a duplicate: %+v", clients)
	}
	for _, c := range clients {
		if c.PublicKey[0] == 1 && (c.SyncSince != 150 || c.OutPathLen != 0xFF || len(c.OutPath) != 0) {
			t.Fatalf("upsert did not update: %+v", c)
		}
	}

	if err := repository.DeleteClient(ctx, "lobby", roomKey(1)); err != nil {
		t.Fatal(err)
	}
	if err := repository.DeleteClient(ctx, "lobby", roomKey(1)); err != nil {
		t.Fatalf("deleting a missing client: %v", err)
	}
	if clients, _ := repository.LoadClients(ctx, "lobby"); len(clients) != 1 || clients[0].PublicKey[0] != 2 {
		t.Fatalf("after delete: %+v", clients)
	}
	if clients, _ := repository.LoadClients(ctx, "other"); len(clients) != 1 {
		t.Fatalf("delete touched another room: %+v", clients)
	}
}

func TestRoomRepositoryConcurrentWritersAndPersistenceDoNotFail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kam.db")
	persistence, err := NewPersistence(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = persistence.Close() })

	const writers = 6
	const postsPerWriter = 30
	repositories := make([]*RoomRepository, writers)
	for i := range repositories {
		repositories[i] = openRoomTestRepository(t, path)
	}
	errs := make([]error, writers)
	var done sync.WaitGroup
	start := make(chan struct{})
	done.Add(writers)
	for i, repository := range repositories {
		go func() {
			defer done.Done()
			<-start
			room := "room-" + strconv.Itoa(i)
			for n := uint32(1); n <= postsPerWriter; n++ {
				if err := repository.AppendPost(context.Background(), room, RoomPost{Timestamp: n, Author: roomKey(1), Text: "x"}, nil, 0); err != nil {
					errs[i] = err
					return
				}
				if err := repository.SaveClients(context.Background(), room, []RoomClient{{PublicKey: roomKey(byte(n)), OutPathLen: 0xFF}}); err != nil {
					errs[i] = err
					return
				}
			}
		}()
	}
	close(start)
	done.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	if count, err := repositories[0].CountPosts(context.Background(), "room-3"); err != nil || count != postsPerWriter {
		t.Fatalf("posts: %d, %v", count, err)
	}
}
