package automation

import (
	"context"
	"errors"
	"sync"
	"testing"

	"go.starlark.net/starlark"

	"kam.bytecoder.nl/internal/db"
	"kam.bytecoder.nl/internal/kiss/parsers"
)

// fakePoster records what a script asked to post and can refuse on demand.
type fakePoster struct {
	mu    sync.Mutex
	calls [][2]string
	err   error
}

func (p *fakePoster) Post(_ context.Context, roomID, text string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, [2]string{roomID, text})
	return p.err
}

func (p *fakePoster) snapshot() [][2]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([][2]string(nil), p.calls...)
}

func runPostCase(t *testing.T, poster RoomPoster, source string) *recordingSender {
	t.Helper()
	repository := openTestRepository(t)
	if _, err := repository.Save(context.Background(), db.AutomationScript{
		ID: "poster", Name: "poster", Source: source, Enabled: true, TimeoutMS: 5000, RuntimeStatus: "READY",
	}, 0); err != nil {
		t.Fatal(err)
	}
	sender := &recordingSender{}
	runtime, err := NewRuntime(context.Background(), repository, sender, poster)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.Stop)
	runtime.Handle(parsers.GroupTextMessage{ChannelName: "public", Body: "go"})
	return sender
}

func TestRoomPostCallsThePoster(t *testing.T) {
	poster := &fakePoster{}
	sender := runPostCase(t, poster, "def on_group_text(message):\n    room_post(\"lobby\", \"weather 4.9 C\")\n    reply(message[\"channel\"], \"posted\")\n")
	got := waitForReplies(t, sender, 1)
	if len(got) != 1 || got[0] != "posted" {
		t.Fatalf("replies = %v", got)
	}
	if calls := poster.snapshot(); len(calls) != 1 || calls[0] != [2]string{"lobby", "weather 4.9 C"} {
		t.Fatalf("poster calls = %v", calls)
	}
}

// A poster error must reach the script so it can decide what to do, and a
// handled error must not freeze the script.
func TestRoomPostErrorIsVisibleToTheScript(t *testing.T) {
	poster := &fakePoster{err: errors.New("no such room")}
	sender := runPostCase(t, poster, "def on_group_text(message):\n    reply(message[\"channel\"], \"start\")\n    room_post(\"lobby\", \"text\")\n")
	waitForReplies(t, sender, 1)
}

func TestRoomPostErrorSurface(t *testing.T) {
	tests := []struct {
		name   string
		poster RoomPoster
		args   starlark.Tuple
		kwargs []starlark.Tuple
		want   string
	}{
		{name: "no poster", args: starlark.Tuple{starlark.String("lobby"), starlark.String("text")}, want: "room_post: rooms are not running"},
		{name: "empty room", poster: &fakePoster{}, args: starlark.Tuple{starlark.String(""), starlark.String("text")}, want: "room_post: room id is required"},
		{name: "empty text", poster: &fakePoster{}, args: starlark.Tuple{starlark.String("lobby"), starlark.String("")}, want: "room_post: text is empty"},
		{name: "missing text", poster: &fakePoster{}, args: starlark.Tuple{starlark.String("lobby")}, want: "room_post: missing argument for text"},
		{name: "wrong type", poster: &fakePoster{}, args: starlark.Tuple{starlark.String("lobby"), starlark.MakeInt(3)}, want: "room_post: for parameter text: got int, want string"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime := &Runtime{poster: test.poster}
			_, err := hostRoomPost(context.Background(), runtime, test.args, test.kwargs)
			if err == nil || err.Error() != test.want {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRoomPostKeywordArguments(t *testing.T) {
	poster := &fakePoster{}
	_, err := hostRoomPost(context.Background(), &Runtime{poster: poster}, nil, []starlark.Tuple{
		{starlark.String("room_id"), starlark.String("lobby")},
		{starlark.String("text"), starlark.String("keyword")},
	})
	if err != nil {
		t.Fatalf("hostRoomPost: %v", err)
	}
	if calls := poster.snapshot(); len(calls) != 1 || calls[0] != [2]string{"lobby", "keyword"} {
		t.Fatalf("poster calls = %v", calls)
	}
}

// room_post is predeclared, so a script can mention it without a runtime
// poster: validation uses a stub.
func TestRoomPostIsPredeclared(t *testing.T) {
	if _, _, err := compile("room-post", "def on_group_text(message):\n    room_post(\"lobby\", \"text\")\n", 1000); err != nil {
		t.Fatalf("compile: %v", err)
	}
}
