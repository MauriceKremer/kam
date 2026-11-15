package room

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"kam.bytecoder.nl/internal/db"
	"kam.bytecoder.nl/internal/models"
)

// storedClient returns the room's stored record for key, or nil.
func (h *harness) storedClient(key [32]byte) *db.RoomClient {
	h.t.Helper()
	stored, err := h.repo.LoadClients(testContext(), h.cfg.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	for i := range stored {
		if bytes.Equal(stored[i].PublicKey, key[:]) {
			return &stored[i]
		}
	}
	return nil
}

func TestRemoveClientForgetsTheRecordAndTheSession(t *testing.T) {
	h := newHarness(t, nil)
	var events []models.MonitorEvent
	h.svc.SetMonitorRecorder(func(event models.MonitorEvent) { events = append(events, event) })
	kept := h.loggedIn(1, "guest-pass")
	gone := h.loggedIn(2, "admin-pass")
	h.svc.flushAll(h.now)
	if h.storedClient(gone.pub()) == nil {
		t.Fatal("setup: the session was not stored")
	}
	tag := h.room.pushTag(h.clientFor(gone))
	events, h.tx.cancels = nil, map[string]int{}

	if err := h.svc.forgetClient("lobby", gone.pub(), h.now); err != nil {
		t.Fatal(err)
	}
	if h.room.byKey[gone.pub()] != nil || len(h.room.clients) != 1 || h.svc.Stats().Clients != 1 {
		t.Fatalf("the session survived: %d clients, gauge %d", len(h.room.clients), h.svc.Stats().Clients)
	}
	if h.storedClient(gone.pub()) != nil || h.storedClient(kept.pub()) == nil {
		t.Fatal("removal must delete exactly the removed client's record")
	}
	if h.tx.cancels[tag] != 1 {
		t.Fatalf("the push tag was not cancelled: %v", h.tx.cancels)
	}
	if len(events) != 1 || events[0].Event != "client_removed" || events[0].Room == nil || *events[0].Room != "lobby" ||
		events[0].Client != hex.EncodeToString(gone.pubSlice()) || events[0].Role != "admin" {
		t.Fatalf("events: %+v", events)
	}

	if err := h.svc.forgetClient("lobby", gone.pub(), h.now); !errors.Is(err, ErrUnknownClient) {
		t.Fatalf("removing twice: %v", err)
	}
	if err := h.svc.forgetClient("annex", kept.pub(), h.now); !errors.Is(err, ErrUnknownRoom) {
		t.Fatalf("unknown room: %v", err)
	}
}

func TestRemoveClientWithdrawsItsPendingPush(t *testing.T) {
	h := newHarness(t, nil)
	author := h.loggedIn(1, "guest-pass")
	reader := h.loggedIn(2, "guest-pass")
	pkt, _ := author.text("news", floodVia())
	h.svc.process(pkt, h.now)
	h.tx.only(t)
	c := h.clientFor(reader)
	tag := h.room.pushTag(c)
	var push sentPacket
	for i := 0; i < 200 && !c.hasPending; i++ {
		h.advance(100 * time.Millisecond)
		for _, sp := range h.tx.take() {
			push = sp
		}
	}
	if !c.hasPending || push.Req.Tag != tag {
		t.Fatal("setup: no push is awaiting its ACK")
	}
	h.tx.queued[tag] = 1 // the scheduler still holds it back
	h.tx.cancels = map[string]int{}

	if err := h.svc.forgetClient("lobby", reader.pub(), h.now); err != nil {
		t.Fatal(err)
	}
	if h.tx.cancels[tag] != 1 || h.tx.queued[tag] != 0 {
		t.Fatalf("the queued push was not withdrawn: cancels %v, queued %v", h.tx.cancels, h.tx.queued)
	}

	// A late ACK matches nobody, the pending push never times out, and nothing
	// more is pushed to the removed client.
	plain := reader.open(push)
	h.svc.process(reader.ackPacket(plain[:9+len("news")], floodVia()), h.now)
	for range 30 {
		h.advance(time.Second)
	}
	h.tx.none(t)
	if stats := h.svc.Stats(); stats.PushesAcked != 0 || stats.PushTimeouts != 0 {
		t.Fatalf("a removed client's push was still tracked: %+v", stats)
	}
}

func TestRemovedClientIsNotResurrectedByALaterBatchWrite(t *testing.T) {
	var flaky *flakyStore
	h := newHarnessWith(t, harnessOptions{store: func(s Store) Store { flaky = &flakyStore{Store: s}; return flaky }})
	kept := h.loggedIn(1, "guest-pass")
	gone := h.loggedIn(2, "guest-pass")
	h.svc.flushAll(h.now)

	// Both sessions change and the batch that should write them fails, so the
	// changes wait for the next batch.
	h.clientFor(kept).syncSince, h.clientFor(kept).dirty = 11, true
	h.clientFor(gone).syncSince, h.clientFor(gone).dirty = 22, true
	flaky.saveErr = os.ErrDeadlineExceeded
	h.advance(clientFlushInterval)
	if !h.clientFor(gone).dirty {
		t.Fatal("setup: the failed batch must leave the change pending")
	}
	flaky.saveErr = nil

	if err := h.svc.forgetClient("lobby", gone.pub(), h.now); err != nil {
		t.Fatal(err)
	}
	saved := flaky.saved
	h.advance(clientFlushInterval)
	if flaky.saved != saved+1 {
		t.Fatal("setup: the retried batch did not run")
	}
	h.clientFor(kept).syncSince, h.clientFor(kept).dirty = 33, true
	h.svc.Stop() // the final flush on shutdown

	if h.storedClient(gone.pub()) != nil {
		t.Fatal("a batch write brought the removed client back")
	}
	if record := h.storedClient(kept.pub()); record == nil || record.SyncSince != 33 {
		t.Fatalf("the other session was not written: %+v", record)
	}
}

func TestFailedDeleteKeepsTheSession(t *testing.T) {
	var flaky *flakyStore
	h := newHarnessWith(t, harnessOptions{store: func(s Store) Store { flaky = &flakyStore{Store: s}; return flaky }})
	var events []models.MonitorEvent
	h.svc.SetMonitorRecorder(func(event models.MonitorEvent) { events = append(events, event) })
	p := h.loggedIn(1, "guest-pass")
	h.svc.flushAll(h.now)
	c := h.clientFor(p)
	c.syncSince, c.dirty = 44, true // a change still waiting for its batch
	events, h.tx.cancels = nil, map[string]int{}

	flaky.deleteErr = os.ErrDeadlineExceeded
	if err := h.svc.forgetClient("lobby", p.pub(), h.now); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("the persistence failure must be returned: %v", err)
	}
	if h.clientFor(p) != c || len(h.room.clients) != 1 || h.svc.Stats().Clients != 1 || len(h.tx.cancels) != 0 || len(events) != 0 {
		t.Fatal("a failed delete changed the session")
	}
	h.advance(clientFlushInterval)
	if record := h.storedClient(p.pub()); record == nil || record.SyncSince != 44 {
		t.Fatalf("the kept session stopped being written: %+v", record)
	}

	flaky.deleteErr = nil
	if err := h.svc.forgetClient("lobby", p.pub(), h.now); err != nil {
		t.Fatal(err)
	}
	if h.storedClient(p.pub()) != nil {
		t.Fatal("the retried removal left the record")
	}
}

func TestRemovedClientCanLogInAgainButNotResume(t *testing.T) {
	h := newHarness(t, nil)
	p := h.loggedIn(1, "guest-pass")
	if err := h.svc.forgetClient("lobby", p.pub(), h.now); err != nil {
		t.Fatal(err)
	}

	// The session is forgotten: a blank-password resume is an unknown sender.
	h.advance(time.Second)
	h.svc.process(p.loginPacket("", 0, direct()), h.now)
	h.tx.none(t)

	// Removal is not a ban: the password still works.
	h.advance(time.Second)
	if response := h.login(p, "guest-pass", 0, floodVia()); response[4] != 0 || response[7] != RoleReadWrite {
		t.Fatalf("login after removal: %x", response)
	}
	if h.clientFor(p).role != RoleReadWrite || h.svc.Stats().Clients != 1 {
		t.Fatalf("stats after the new login: %+v", h.svc.Stats())
	}
}

func TestRemoveClientRunsOnTheWorker(t *testing.T) {
	h := newHarness(t, nil)
	p := h.loggedIn(1, "guest-pass")
	h.svc.Start()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := h.svc.RemoveClient(ctx, "lobby", p.pub()); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.RemoveClient(ctx, "lobby", p.pub()); !errors.Is(err, ErrUnknownClient) {
		t.Fatalf("removing twice: %v", err)
	}
	snapshot, err := h.svc.MonitorSnapshot(ctx)
	if err != nil || len(snapshot[0].Clients) != 0 {
		t.Fatalf("snapshot after removal: %+v, %v", snapshot, err)
	}
	h.svc.Stop()
	if err := h.svc.RemoveClient(ctx, "lobby", p.pub()); err == nil || errors.Is(err, ErrUnknownClient) {
		t.Fatalf("a stopped service must refuse: %v", err)
	}

	// Without a worker the caller's context bounds the wait.
	idle := newHarness(t, nil)
	short, cancelShort := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelShort()
	if err := idle.svc.RemoveClient(short, "lobby", p.pub()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a request nobody takes: %v", err)
	}
}
