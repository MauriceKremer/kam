package monitor

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEventsPaginationAndReset(t *testing.T) {
	ring := NewRing(3)
	for range 5 {
		ring.Record(Event{Kind: "rx", At: time.Now(), PayloadType: "TRACE"})
	}
	handler := NewHandler(ring, func() Status { return Status{NodeName: "test"} })
	read := func(query string, code int) EventsResponse {
		t.Helper()
		request := httptest.NewRequest("GET", "http://127.0.0.1/v1/events"+query, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != code {
			t.Fatalf("%s: status %d: %s", query, response.Code, response.Body)
		}
		var page EventsResponse
		if code == http.StatusOK {
			if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
				t.Fatal(err)
			}
		}
		return page
	}
	first := read("?after=0&limit=2", 200)
	if first.Reset || first.FirstSeq != 3 || first.LastSeq != 5 || first.NextAfter != 4 || len(first.Events) != 2 {
		t.Fatalf("first page: %+v", first)
	}
	last := read("?after=4&epoch="+first.Epoch, 200)
	if last.NextAfter != 5 || len(last.Events) != 1 {
		t.Fatalf("last page: %+v", last)
	}
	if page := read("?after=5&epoch="+first.Epoch, 200); len(page.Events) != 0 || page.NextAfter != 5 {
		t.Fatalf("caught up: %+v", page)
	}
	if page := read("?after=1&epoch="+first.Epoch, 200); !page.Reset || page.Events[0].Seq != 3 {
		t.Fatalf("overwritten cursor: %+v", page)
	}
	if page := read("?after=900&epoch=old-run", 200); !page.Reset || page.Events[0].Seq != 3 {
		t.Fatalf("restart: %+v", page)
	}
	for _, query := range []string{"?after=-1", "?after=6&epoch=" + first.Epoch, "?after=1", "?limit=2001", "?limit=0"} {
		read(query, 400)
	}
}

type testChannelReader struct {
	channels []Channel
	page     ChannelPage
	got      struct {
		name, query string
		limit       int
		beforeID    int64
	}
}

type testRoomReader struct {
	rooms   []Room
	clients []RoomClient
	posts   []RoomPost
	limit   int
}

func (r *testRoomReader) Rooms(context.Context) ([]Room, error) { return r.rooms, nil }
func (r *testRoomReader) Clients(context.Context, string) ([]RoomClient, error) {
	return r.clients, nil
}
func (r *testRoomReader) Posts(_ context.Context, _ string, limit int) ([]RoomPost, error) {
	r.limit = limit
	return r.posts, nil
}

func (r *testChannelReader) Channels(_ context.Context) ([]Channel, error) {
	return r.channels, nil
}

func (r *testChannelReader) Messages(_ context.Context, name string, limit int, beforeID int64, query string) (ChannelPage, error) {
	r.got.name, r.got.limit, r.got.beforeID, r.got.query = name, limit, beforeID, query
	return r.page, nil
}

func TestChannelEndpointsAndValidation(t *testing.T) {
	reader := &testChannelReader{
		channels: []Channel{{Name: "#emmen", KeyStrategy: "derived", Enabled: true, StoredMessages: 1}},
		page:     ChannelPage{Messages: []ChannelMessage{}, NextBeforeID: nil},
	}
	handler := NewHandlerWithChannels(NewRing(256), func() Status { return Status{} }, reader)
	request := func(path string, code int) string {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", "http://127.0.0.1"+path, nil))
		if response.Code != code {
			t.Fatalf("%s: got %d: %s", path, response.Code, response.Body)
		}
		return response.Body.String()
	}
	if body := request("/v1/channels", 200); !strings.Contains(body, `"channels":[{"name":"#emmen"`) || strings.Contains(body, "key_hex") {
		t.Fatalf("channel response leaked/omitted fields: %s", body)
	}
	if body := request("/v1/channels/%23emmen/messages?limit=12&before_id=9&q=%25_%C3%A9", 200); !strings.Contains(body, `"channel":"#emmen"`) || !strings.Contains(body, `"messages":[]`) || !strings.Contains(body, `"next_before_id":null`) {
		t.Fatalf("message response: %s", body)
	}
	if reader.got.name != "#emmen" || reader.got.limit != 12 || reader.got.beforeID != 9 || reader.got.query != "%_é" {
		t.Fatalf("provider args: %+v", reader.got)
	}
	request("/v1/channels/%23missing/messages", 404)
	for _, path := range []string{
		"/v1/channels/%23emmen/messages?limit=201",
		"/v1/channels/%23emmen/messages?before_id=-1",
		"/v1/channels/%23emmen/messages?before_id=0",
		"/v1/channels/%23emmen/messages?q=" + strings.Repeat("x", 201),
	} {
		request(path, 400)
	}
}

func TestRoomReadEndpoints(t *testing.T) {
	reader := &testRoomReader{
		rooms:   []Room{{ID: "lobby", Name: "Lobby", Enabled: true, ClientsRemembered: 1}},
		clients: []RoomClient{},
		posts:   []RoomPost{{ID: 7, Body: "hello"}},
	}
	handler := NewHandlerWithResources(NewRing(256), func() Status { return Status{} }, nil, reader)
	request := func(path string, code int) string {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", "http://127.0.0.1"+path, nil))
		if response.Code != code {
			t.Fatalf("%s: got %d: %s", path, response.Code, response.Body)
		}
		return response.Body.String()
	}
	if body := request("/v1/rooms", 200); !strings.Contains(body, `"rooms":[{"id":"lobby"`) {
		t.Fatalf("room response: %s", body)
	}
	if body := request("/v1/rooms/lobby/clients", 200); !strings.Contains(body, `"room":"lobby"`) || !strings.Contains(body, `"clients":[]`) {
		t.Fatalf("client response: %s", body)
	}
	if body := request("/v1/rooms/lobby/posts?limit=20", 200); !strings.Contains(body, `"posts":[{"id":7`) || reader.limit != 20 {
		t.Fatalf("posts response: %s, limit %d", body, reader.limit)
	}
	request("/v1/rooms/missing/posts", 404)
	request("/v1/rooms/lobby/posts?limit=201", 400)
	disabled := NewHandler(NewRing(256), func() Status { return Status{} })
	response := httptest.NewRecorder()
	disabled.ServeHTTP(response, httptest.NewRequest("GET", "http://127.0.0.1/v1/rooms", nil))
	if response.Code != 503 {
		t.Fatalf("disabled rooms status %d", response.Code)
	}
}

func TestEmptyStatusHostAndListenerGuards(t *testing.T) {
	ring := NewRing(256)
	handler := NewHandler(ring, func() Status { return Status{NodeName: "node"} })
	for _, test := range []struct {
		url  string
		code int
	}{
		{"http://127.0.0.1/v1/events", 200}, {"http://[::1]/v1/status", 200}, {"http://evil.example/v1/status", 403},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", test.url, nil))
		if response.Code != test.code {
			t.Fatalf("%s: %d", test.url, response.Code)
		}
		if test.code == 200 && response.Header().Get("Content-Type") != "application/json" {
			t.Fatal("missing JSON content type")
		}
	}
	page, err := ring.page(0, "", 1000)
	if err != nil || page.FirstSeq != 0 || page.LastSeq != 0 || page.NextAfter != 0 || page.Events == nil {
		t.Fatalf("empty page: %+v %v", page, err)
	}
	if page, _ = ring.page(100, "old-epoch", 1000); !page.Reset || page.NextAfter != 0 {
		t.Fatalf("empty reset: %+v", page)
	}
	if server, err := Listen("0.0.0.0:0", handler); err == nil {
		server.Close()
		t.Fatal("non-loopback bind accepted")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if _, err := Listen(listener.Addr().String(), handler); err == nil {
		t.Fatal("occupied port should fail")
	}
}

type slowResponse struct {
	header  http.Header
	entered chan struct{}
	release chan struct{}
}

func (w *slowResponse) Header() http.Header { return w.header }
func (w *slowResponse) WriteHeader(int)     {}
func (w *slowResponse) Write(body []byte) (int, error) {
	close(w.entered)
	<-w.release
	return len(body), nil
}

func TestSlowReaderDoesNotHoldRingLock(t *testing.T) {
	ring := NewRing(256)
	ring.Record(Event{Kind: "rx", At: time.Now()})
	handler := NewHandler(ring, func() Status { return Status{} })
	response := &slowResponse{header: make(http.Header), entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(response, httptest.NewRequest("GET", "http://127.0.0.1/v1/events", nil))
	}()
	<-response.entered
	recorded := make(chan struct{})
	go func() { ring.Record(Event{Kind: "rx", At: time.Now()}); close(recorded) }()
	select {
	case <-recorded:
	case <-time.After(time.Second):
		t.Error("network writer retained ring lock")
	}
	close(response.release)
	<-done
}

func TestRingOwnsCapturedValues(t *testing.T) {
	ring := NewRing(256)
	name := "before"
	snr := 8.5
	ring.Record(Event{At: time.Now(), Name: &name, SNRDB: &snr})
	name = "after"
	snr = 0
	page, _ := ring.page(0, "", 100)
	if *page.Events[0].Name != "before" || *page.Events[0].SNRDB != 8.5 {
		t.Fatal("captured values mutated")
	}
}

func TestConcurrentRequestLimit(t *testing.T) {
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	handler := NewHandler(NewRing(256), func() Status { entered <- struct{}{}; <-release; return Status{} })
	var requests sync.WaitGroup
	for range 8 {
		requests.Add(1)
		go func() {
			defer requests.Done()
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://127.0.0.1/v1/status", nil))
		}()
	}
	for range 8 {
		<-entered
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "http://127.0.0.1/v1/status", nil))
	if response.Code != 503 {
		t.Errorf("limit response %d", response.Code)
	}
	close(release)
	requests.Wait()
}

func BenchmarkRingRecordWithReaders(b *testing.B) {
	ring := NewRing(8192)
	for range 8192 {
		ring.Record(Event{Kind: "rx", At: time.Now()})
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				ring.page(0, "", 1000)
			}
		}
	}()
	b.ResetTimer()
	for b.Loop() {
		ring.Record(Event{Kind: "rx", At: time.Now()})
	}
	b.StopTimer()
	close(stop)
	<-done
}
