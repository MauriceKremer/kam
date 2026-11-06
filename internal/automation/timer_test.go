package automation

import (
	"context"
	"sync"
	"testing"
	"time"

	"kam.bytecoder.nl/internal/db"
	"kam.bytecoder.nl/internal/kiss/parsers"
)

// fastTimers makes one declared minute take unit of real time and restores the
// normal behaviour afterwards.
func fastTimers(t *testing.T, unit time.Duration) {
	t.Helper()
	previous := timerUnit
	timerUnit = unit
	t.Cleanup(func() { timerUnit = previous })
}

func saveTimerScript(t *testing.T, repository Repository, id, source string) {
	t.Helper()
	if _, err := repository.Save(context.Background(), db.AutomationScript{
		ID: id, Name: id, Source: source, Enabled: true, TimeoutMS: 5000, RuntimeStatus: "READY",
	}, 0); err != nil {
		t.Fatalf("save %s: %v", id, err)
	}
}

const timerEveryMinute = "timer_interval_minutes = 1\ndef on_timer(message):\n    room_post(\"lobby\", \"tick\")\n"

// countPoster records how many times it was called and when.
type countPoster struct {
	mu    sync.Mutex
	calls []time.Time
	texts []string
}

func (p *countPoster) Post(_ context.Context, _, text string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, time.Now())
	p.texts = append(p.texts, text)
	return nil
}

func (p *countPoster) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

// textsAt returns the nth posted text; the lock keeps a race detector run quiet.
func (p *countPoster) textsAt(index int) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.texts[index]
}

func (p *countPoster) times() []time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]time.Time(nil), p.calls...)
}

// waitForCount waits until the poster has been called at least n times.
func (p *countPoster) waitForCount(t *testing.T, n int, limit time.Duration) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if p.count() >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timer fired %d times, want %d", p.count(), n)
}

func newTimerRuntime(t *testing.T, repository Repository, poster RoomPoster) *Runtime {
	t.Helper()
	runtime, err := NewRuntime(context.Background(), repository, &recordingSender{}, poster)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.Stop)
	return runtime
}

func TestTimerFiresOnItsInterval(t *testing.T) {
	fastTimers(t, 40*time.Millisecond)
	repository := openTestRepository(t)
	poster := &countPoster{}
	saveTimerScript(t, repository, "weather", timerEveryMinute)
	newTimerRuntime(t, repository, poster)

	poster.waitForCount(t, 2, 2*time.Second)
	times := poster.times()
	if gap := times[1].Sub(times[0]); gap < 20*time.Millisecond {
		t.Fatalf("second fire came %v after the first; the interval is 40ms", gap)
	}
	if texts := poster.texts; texts[0] != "tick" {
		t.Fatalf("posted %q", texts)
	}
}

// The first fire is one full interval after the runtime starts, never immediate.
func TestTimerDoesNotFireImmediately(t *testing.T) {
	fastTimers(t, 150*time.Millisecond)
	repository := openTestRepository(t)
	poster := &countPoster{}
	saveTimerScript(t, repository, "weather", timerEveryMinute)
	newTimerRuntime(t, repository, poster)

	time.Sleep(70 * time.Millisecond)
	if got := poster.count(); got != 0 {
		t.Fatalf("fired %d times before the first interval elapsed", got)
	}
	poster.waitForCount(t, 1, 2*time.Second)
}

func TestDisabledTimerScriptStopsFiring(t *testing.T) {
	fastTimers(t, 40*time.Millisecond)
	repository := openTestRepository(t)
	poster := &countPoster{}
	saveTimerScript(t, repository, "weather", timerEveryMinute)
	runtime := newTimerRuntime(t, repository, poster)
	poster.waitForCount(t, 1, 2*time.Second)

	ctx := context.Background()
	definition, err := repository.Get(ctx, "weather")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.SetEnabled(ctx, "weather", false, definition.Revision); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond) // let a fire already in flight finish
	before := poster.count()
	time.Sleep(200 * time.Millisecond)
	if after := poster.count(); after != before {
		t.Fatalf("a disabled script fired %d more times", after-before)
	}
}

func TestDeletedTimerScriptStopsFiring(t *testing.T) {
	fastTimers(t, 40*time.Millisecond)
	repository := openTestRepository(t)
	poster := &countPoster{}
	saveTimerScript(t, repository, "weather", timerEveryMinute)
	runtime := newTimerRuntime(t, repository, poster)
	poster.waitForCount(t, 1, 2*time.Second)

	ctx := context.Background()
	definition, err := repository.Get(ctx, "weather")
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Delete(ctx, "weather", definition.Revision); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	before := poster.count()
	time.Sleep(200 * time.Millisecond)
	if after := poster.count(); after != before {
		t.Fatalf("a deleted script fired %d more times", after-before)
	}
}

// A shorter interval takes effect at the next save, measured from the save.
func TestSavingANewIntervalRestartsTheCycle(t *testing.T) {
	fastTimers(t, 100*time.Millisecond)
	repository := openTestRepository(t)
	poster := &countPoster{}
	saveTimerScript(t, repository, "weather", "timer_interval_minutes = 10\ndef on_timer(message):\n    room_post(\"lobby\", \"slow\")\n")
	runtime := newTimerRuntime(t, repository, poster)

	time.Sleep(60 * time.Millisecond)
	if got := poster.count(); got != 0 {
		t.Fatalf("the slow script fired %d times", got)
	}
	definition, err := repository.Get(context.Background(), "weather")
	if err != nil {
		t.Fatal(err)
	}
	definition.Source = timerEveryMinute
	if _, err := runtime.Save(context.Background(), definition, definition.Revision); err != nil {
		t.Fatal(err)
	}
	poster.waitForCount(t, 1, 2*time.Second)
}

// A frozen script leaves the schedule and rejoins one full interval after it is
// recovered.
func TestFrozenTimerScriptLeavesTheScheduleAndRejoins(t *testing.T) {
	fastTimers(t, 40*time.Millisecond)
	repository := openTestRepository(t)
	poster := &countPoster{}
	if _, err := repository.Save(context.Background(), db.AutomationScript{
		ID: "slow", Name: "slow", TimeoutMS: 50, Enabled: true, RuntimeStatus: "READY",
		Source: "timer_interval_minutes = 1\ndef on_timer(message):\n    for index in range(1000000000):\n        pass\n",
	}, 0); err != nil {
		t.Fatal(err)
	}
	runtime, err := NewRuntime(context.Background(), repository, &recordingSender{}, poster)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.Stop)

	// Wait for the freeze, then check the schedule is empty.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		definition, err := repository.Get(context.Background(), "slow")
		if err == nil && definition.RuntimeStatus == "FROZEN" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if definition, err := repository.Get(context.Background(), "slow"); err != nil || definition.RuntimeStatus != "FROZEN" {
		t.Fatalf("status = %+v, %v", definition, err)
	}
	// The declaration is still reported, but the schedule no longer fires it.
	if intervals := runtime.TimerIntervals(); intervals["slow"] != 1 {
		t.Fatalf("intervals = %v", intervals)
	}
	posts := poster.count()
	time.Sleep(150 * time.Millisecond)
	if after := poster.count(); after != posts {
		t.Fatalf("a frozen script fired %d more times", after-posts)
	}

	revision := currentRevision(t, repository, "slow")
	if _, err := runtime.Recover(context.Background(), "slow", 10, revision); err != nil {
		t.Fatal(err)
	}
	if intervals := runtime.TimerIntervals(); intervals["slow"] != 1 {
		t.Fatalf("recovered script intervals = %v", intervals)
	}
}

func currentRevision(t *testing.T, repository Repository, id string) int64 {
	t.Helper()
	current, err := repository.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return current.Revision
}

// A timer-only script never runs on a group text, even though it is ready.
func TestTimerOnlyScriptIgnoresGroupText(t *testing.T) {
	repository := openTestRepository(t)
	poster := &countPoster{}
	sender := &recordingSender{}
	saveTimerScript(t, repository, "weather", "timer_interval_minutes = 60\ndef on_timer(message):\n    reply(\"public\", \"timer ran\")\n")
	runtime, err := NewRuntime(context.Background(), repository, sender, poster)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.Stop)
	runtime.Handle(parsers.GroupTextMessage{ChannelName: "public", Body: "hello"})
	time.Sleep(100 * time.Millisecond)
	if got := sender.snapshot(); len(got) != 0 {
		t.Fatalf("a timer-only script replied to a group text: %v", got)
	}
}

// A script with both entries runs both paths: messages on group texts, timers
// on its interval.
func TestScriptWithBothEntriesRunsBothPaths(t *testing.T) {
	fastTimers(t, 40*time.Millisecond)
	repository := openTestRepository(t)
	sender := &recordingSender{}
	source := "timer_interval_minutes = 1\n" +
		"def on_group_text(message):\n    reply(message[\"channel\"], \"text\")\n" +
		"def on_timer(message):\n    room_post(\"lobby\", \"timer %d\" % message[\"timestamp_unix\"])\n"
	saveTimerScript(t, repository, "both", source)
	poster := &countPoster{}
	runtime, err := NewRuntime(context.Background(), repository, sender, poster)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.Stop)
	runtime.Handle(parsers.GroupTextMessage{ChannelName: "public", Body: "hello"})
	if replies := waitForReplies(t, sender, 1); replies[0] != "text" {
		t.Fatalf("replies = %v", replies)
	}
	poster.waitForCount(t, 1, 2*time.Second)
}

// TimerIntervals reports the declared period of every timer script, enabled or
// not, so the API can show it.
func TestTimerIntervalsReportsDeclaredPeriods(t *testing.T) {
	repository := openTestRepository(t)
	saveTimerScript(t, repository, "weather", timerEveryMinute)
	saveTimerScript(t, repository, "chatty", "def on_group_text(message):\n    reply(message[\"channel\"], \"hi\")\n")
	runtime := newTimerRuntime(t, repository, &countPoster{})
	intervals := runtime.TimerIntervals()
	if len(intervals) != 1 || intervals["weather"] != 1 {
		t.Fatalf("intervals = %v", intervals)
	}
}

// Stop returns promptly and leaves no goroutine trying to fire.
func TestStopEndsTheTimerLoop(t *testing.T) {
	fastTimers(t, 20*time.Millisecond)
	repository := openTestRepository(t)
	poster := &countPoster{}
	saveTimerScript(t, repository, "weather", timerEveryMinute)
	runtime, err := NewRuntime(context.Background(), repository, &recordingSender{}, poster)
	if err != nil {
		t.Fatal(err)
	}
	poster.waitForCount(t, 1, 2*time.Second)
	runtime.Stop()
	before := poster.count()
	time.Sleep(100 * time.Millisecond)
	if after := poster.count(); after != before {
		t.Fatalf("fires continued after Stop: %d more", after-before)
	}
}

func TestTimerFireDropIncrementsTheDroppedCounter(t *testing.T) {
	fastTimers(t, 20*time.Millisecond)
	repository := openTestRepository(t)
	saveTimerScript(t, repository, "weather", "timer_interval_minutes = 1\ndef on_timer(message):\n    for index in range(200000000):\n        pass\n")
	runtime, err := NewRuntimeWithQueueSize(context.Background(), repository, &recordingSender{}, &countPoster{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.Stop)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, _, dropped := runtime.MonitorStats()
		if dropped > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, depth, dropped := runtime.MonitorStats()
	t.Fatalf("no drop was counted with a full queue (depth %d, dropped %d)", depth, dropped)
}
