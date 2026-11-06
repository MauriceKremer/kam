package automation

import (
	"context"
	"strings"
	"testing"
	"time"

	"kam.bytecoder.nl/internal/kiss/parsers"
)

const messageScript = "def on_group_text(message):\n    pass\n"
const timerScript = "timer_interval_minutes = 5\n\ndef on_timer(message):\n    pass\n"

func TestIntervalLiteralGrammar(t *testing.T) {
	cases := []struct {
		name    string
		source  string
		want    int64
		wantErr string
	}{
		{"absent", messageScript, 0, ""},
		{"integer literal", "timer_interval_minutes = 15\n\ndef on_timer(message):\n    pass\n", 15, ""},
		{"zero means no timer", "timer_interval_minutes = 0\n" + messageScript, 0, ""},
		{"duplicate assignment is rejected by Starlark", "timer_interval_minutes = 5\ntimer_interval_minutes = 9\n\ndef on_timer(message):\n    pass\n", 0, "cannot reassign global timer_interval_minutes"},
		{"float literal", "timer_interval_minutes = 5.0\n\ndef on_timer(message):\n    pass\n", 0, "timer_interval_minutes must be an integer literal number"},
		{"string literal", "timer_interval_minutes = \"5\"\n\ndef on_timer(message):\n    pass\n", 0, "timer_interval_minutes must be an integer literal number"},
		{"expression", "timer_interval_minutes = 2 + 3\n\ndef on_timer(message):\n    pass\n", 0, "timer_interval_minutes must be an integer literal number"},
		{"function call", "timer_interval_minutes = int(5)\n\ndef on_timer(message):\n    pass\n", 0, "timer_interval_minutes must be an integer literal number"},
		{"augmented assignment is rejected by Starlark", "timer_interval_minutes = 5\ntimer_interval_minutes += 1\n\ndef on_timer(message):\n    pass\n", 0, "cannot reassign global timer_interval_minutes"},
		{"negative", "timer_interval_minutes = -5\n\ndef on_timer(message):\n    pass\n", 0, "timer_interval_minutes must be at least 1"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, compiled, err := compileWithContext(context.Background(), "grammar", test.source, 500)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error: got %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if compiled.interval != test.want {
				t.Fatalf("interval: got %d, want %d", compiled.interval, test.want)
			}
		})
	}
}

func TestIntervalFloorUsesMinTimerInterval(t *testing.T) {
	previous := minTimerInterval
	minTimerInterval = 10 * time.Minute
	t.Cleanup(func() { minTimerInterval = previous })
	_, _, err := compileWithContext(context.Background(), "floor", "timer_interval_minutes = 5\n\ndef on_timer(message):\n    pass\n", 500)
	if err == nil || !strings.Contains(err.Error(), "timer_interval_minutes must be at least 1") {
		t.Fatalf("error: got %v, want a floor error", err)
	}
	_, compiled, err := compileWithContext(context.Background(), "floor-ok", "timer_interval_minutes = 10\n\ndef on_timer(message):\n    pass\n", 500)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if compiled.interval != 10 {
		t.Fatalf("interval: got %d, want 10", compiled.interval)
	}
}

func TestEntryPointRuleMatrix(t *testing.T) {
	cases := []struct {
		name    string
		source  string
		message bool
		timer   bool
		wantErr string
	}{
		{"message only", messageScript, true, false, ""},
		{"timer only", timerScript, false, true, ""},
		{"both", "timer_interval_minutes = 5\n\ndef on_group_text(message):\n    pass\n\ndef on_timer(message):\n    pass\n", true, true, ""},
		{"neither", "VALUE = 1\n", false, false, "script must define callable on_group_text(message) or a timer"},
		{"interval without on_timer", "timer_interval_minutes = 5\n", false, false, "script sets timer_interval_minutes but does not define callable on_timer(message)"},
		{"on_timer without interval", "def on_timer(message):\n    pass\n", false, true, "script defines on_timer but does not set timer_interval_minutes"},
		{"on_timer is not callable", "timer_interval_minutes = 5\non_timer = 1\n", false, false, "script sets timer_interval_minutes but does not define callable on_timer(message)"},
		{"on_group_text is not callable", "on_group_text = 1\n", false, false, "script must define callable on_group_text(message) or a timer"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, compiled, err := compileWithContext(context.Background(), "matrix", test.source, 500)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error: got %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if compiled.messageEntry != test.message || compiled.timerEntry != test.timer {
				t.Fatalf("entries: got message=%v timer=%v, want message=%v timer=%v", compiled.messageEntry, compiled.timerEntry, test.message, test.timer)
			}
		})
	}
}

func TestLibrariesArePredeclared(t *testing.T) {
	source := "PAYLOAD = json.decode('{\"temp_c\": 4.9}')\nSTAMP = time.from_timestamp(1793989800)\n\ndef on_group_text(message):\n    reply(message[\"channel\"], \"%s %s\" % (STAMP.unix, PAYLOAD[\"temp_c\"]))\n"
	repository := openTestRepository(t)
	saveScript(t, repository, "libs", source)
	sender := &recordingSender{}
	runtime := newTestRuntime(t, repository, sender)
	runtime.Handle(parsers.GroupTextMessage{ChannelName: "public", Body: "go"})
	got := waitForReplies(t, sender, 1)
	if got[0] != "1793989800 4.9" {
		t.Fatalf("reply: got %q, want %q", got[0], "1793989800 4.9")
	}
}

func TestLibrariesAreNotAssignablePredeclaredNames(t *testing.T) {
	// json and time resolve as predeclared names, so a script may shadow them.
	source := "json = \"mine\"\n\ndef on_group_text(message):\n    reply(message[\"channel\"], json)\n"
	repository := openTestRepository(t)
	saveScript(t, repository, "shadow", source)
	sender := &recordingSender{}
	runtime := newTestRuntime(t, repository, sender)
	runtime.Handle(parsers.GroupTextMessage{ChannelName: "public", Body: "go"})
	got := waitForReplies(t, sender, 1)
	if got[0] != "mine" {
		t.Fatalf("reply: got %q, want %q", got[0], "mine")
	}
}

func TestTimerOnlyScriptDoesNotReceiveGroupText(t *testing.T) {
	repository := openTestRepository(t)
	saveScript(t, repository, "timer", "timer_interval_minutes = 5\n\ndef on_timer(message):\n    pass\n")
	sender := &recordingSender{}
	runtime := newTestRuntime(t, repository, sender)
	runtime.Handle(parsers.GroupTextMessage{ChannelName: "public", Body: "go"})
	time.Sleep(50 * time.Millisecond)
	if got := sender.snapshot(); len(got) != 0 {
		t.Fatalf("timer-only script ran for a group text: %v", got)
	}
}
