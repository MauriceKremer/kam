package automation

import (
	"context"
	"strings"
	"testing"
	"time"
)

const cronEverySecond = "timer_schedule = \"*/1 * * * * *\"\ndef on_timer(now):\n    room_post(\"lobby\", \"tick\")\n"

// A cron-declared timer fires on its slots: the first fire is one full slot
// after the runtime starts, never immediate, and slots repeat.
func TestTimerScheduleFiresOnTheCronSlot(t *testing.T) {
	repository := openTestRepository(t)
	poster := &countPoster{}
	saveTimerScript(t, repository, "cron", cronEverySecond)
	newTimerRuntime(t, repository, poster)

	poster.waitForCount(t, 2, 6*time.Second)
	times := poster.times()
	if gap := times[1].Sub(times[0]); gap < 500*time.Millisecond {
		t.Fatalf("second fire came %v after the first; the schedule is one per second", gap)
	}
	if texts := poster.texts; texts[0] != "tick" {
		t.Fatalf("posted %q", texts)
	}
}

func TestTimerScheduleDoesNotFireImmediately(t *testing.T) {
	repository := openTestRepository(t)
	poster := &countPoster{}
	saveTimerScript(t, repository, "cron", cronEverySecond)
	newTimerRuntime(t, repository, poster)

	time.Sleep(500 * time.Millisecond)
	if got := poster.count(); got != 0 {
		t.Fatalf("fired %d times before the first cron slot", got)
	}
	poster.waitForCount(t, 1, 3*time.Second)
}

// A cron-declared script leaves the schedule when it is disabled and rejoins
// when it is enabled again.
func TestDisabledCronScriptStopsFiring(t *testing.T) {
	repository := openTestRepository(t)
	poster := &countPoster{}
	saveTimerScript(t, repository, "cron", cronEverySecond)
	runtime := newTimerRuntime(t, repository, poster)
	poster.waitForCount(t, 1, 6*time.Second)

	ctx := context.Background()
	definition, err := repository.Get(ctx, "cron")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.SetEnabled(ctx, "cron", false, definition.Revision); err != nil {
		t.Fatal(err)
	}
	before := poster.count()
	time.Sleep(1500 * time.Millisecond)
	if after := poster.count(); after != before {
		t.Fatalf("a disabled script fired %d more times", after-before)
	}
}

// TimerSchedules reports the declared schedule text of every cron script,
// enabled or not, so the API can show it.
func TestTimerSchedulesReportsDeclaredSchedules(t *testing.T) {
	repository := openTestRepository(t)
	saveTimerScript(t, repository, "weather", cronEverySecond)
	saveTimerScript(t, repository, "chatty", "def on_group_text(message):\n    reply(message[\"channel\"], \"hi\")\n")
	runtime := newTimerRuntime(t, repository, &countPoster{})
	schedules := runtime.TimerSchedules()
	if len(schedules) != 1 || schedules["weather"] != "*/1 * * * * *" {
		t.Fatalf("schedules = %v", schedules)
	}
	if intervals := runtime.TimerIntervals(); len(intervals) != 0 {
		t.Fatalf("intervals = %v, want none for cron-declared scripts", intervals)
	}
}

// The timer_schedule compile contract: a plain string literal holding a valid
// expression, no second timer declaration, and the on_timer entry point.
func TestTimerScheduleCompileContract(t *testing.T) {
	messageScript := "def on_group_text(message):\n    pass\n"
	cases := []struct {
		name   string
		source string
		reason string
	}{
		{"valid five fields", "timer_schedule = \"0 7 * * *\"\ndef on_timer(now):\n    pass\n", ""},
		{"valid with seconds", "timer_schedule = \"*/5 * * * * *\"\ndef on_timer(now):\n    pass\n", ""},
		{"empty string is no timer", "timer_schedule = \"\"\n" + messageScript, ""},
		{"not a string", "timer_schedule = 5\n\ndef on_timer(now):\n    pass\n", "timer_schedule must be a string literal cron expression"},
		{"expression value", "timer_schedule = \"0 7 * * \" + \"*\"\n\ndef on_timer(now):\n    pass\n", "timer_schedule must be a string literal cron expression"},
		{"invalid expression", "timer_schedule = \"* * *\"\ndef on_timer(now):\n    pass\n", "is not a valid cron expression"},
		{"both declarations", "timer_interval_minutes = 5\ntimer_schedule = \"0 7 * * *\"\ndef on_timer(now):\n    pass\n", "must not set both timer_interval_minutes and timer_schedule"},
		{"timer without either", "def on_timer(now):\n    pass\n", "does not set timer_interval_minutes or timer_schedule"},
		{"schedule without entry", "timer_schedule = \"0 7 * * *\"\nVALUE = 1\n", "sets timer_schedule but does not define a callable on_timer"},
	}
	for _, test := range cases {
		_, _, err := compile("cron", test.source, 1000)
		if test.reason == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", test.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), test.reason) {
			t.Errorf("%s: error %v, want it to contain %q", test.name, err, test.reason)
		}
	}
}

// A save that changes the schedule must replace the pending fire, not keep the
// old one: a script parked on a far-away slot starts firing on the new one.
func TestChangingTheScheduleReplacesThePendingFire(t *testing.T) {
	repository := openTestRepository(t)
	poster := &countPoster{}
	saveTimerScript(t, repository, "cron", "timer_schedule = \"0 0 1 1 *\"\ndef on_timer(now):\n    room_post(\"lobby\", \"tick\")\n")
	runtime := newTimerRuntime(t, repository, poster)

	time.Sleep(150 * time.Millisecond)
	if got := poster.count(); got != 0 {
		t.Fatalf("a January-1st script fired %d times", got)
	}
	definition, err := repository.Get(context.Background(), "cron")
	if err != nil {
		t.Fatal(err)
	}
	definition.Source = cronEverySecond
	if _, err := runtime.Save(context.Background(), definition, definition.Revision); err != nil {
		t.Fatal(err)
	}
	// The new schedule is due one second after the save; the old pending slot
	// was January 1st.
	poster.waitForCount(t, 1, 3*time.Second)
}

// Same rule for the interval form: the new interval is measured from the save,
// not from the period the script was running before.
func TestShorteningAnIntervalTakesEffectAtTheSave(t *testing.T) {
	fastTimers(t, 100*time.Millisecond) // one declared minute = 100ms
	repository := openTestRepository(t)
	poster := &countPoster{}
	saveTimerScript(t, repository, "weather", "timer_interval_minutes = 60\ndef on_timer(now):\n    room_post(\"lobby\", \"tick\")\n")
	runtime := newTimerRuntime(t, repository, poster)

	time.Sleep(120 * time.Millisecond)
	definition, err := repository.Get(context.Background(), "weather")
	if err != nil {
		t.Fatal(err)
	}
	definition.Source = timerEveryMinute // 1 minute = 100ms, replacing 60 minutes
	saved := time.Now()
	if _, err := runtime.Save(context.Background(), definition, definition.Revision); err != nil {
		t.Fatal(err)
	}
	poster.waitForCount(t, 1, 2*time.Second)
	// The old pending fire was ~6s away; the new one is 100ms after the save.
	if elapsed := time.Since(saved); elapsed > time.Second {
		t.Fatalf("fired %v after the save, want the new interval", elapsed)
	}
}
