package automation

import (
	"os"
	"reflect"
	"testing"
	"time"

	"kam.bytecoder.nl/internal/kiss/parsers"
)

// The example script replaced the native ECHO service, so these cases pin its exact replies.
func TestEchoExampleScript(t *testing.T) {
	source, err := os.ReadFile("../../docs/examples/echo.star")
	if err != nil {
		t.Fatal(err)
	}
	repository := openTestRepository(t)
	saveScript(t, repository, "echo", string(source))
	sender := &recordingSender{}
	runtime := newTestRuntime(t, repository, sender)

	post := func(channel, from, body string, hops int) parsers.GroupTextMessage {
		return parsers.GroupTextMessage{ChannelName: channel, Sender: from, Body: body, HopCount: hops}
	}
	withRadio := func(message parsers.GroupTextMessage, snr, rssi int, drift int64) parsers.GroupTextMessage {
		message.HasRxMeta, message.SNR, message.RSSI, message.TimeDriftSeconds = true, snr, rssi, drift
		return message
	}
	drifting := post("#public", "alice", "@kam", 0)
	drifting.TimeDriftSeconds = 7

	cases := []struct {
		name    string
		message parsers.GroupTextMessage
		want    string // empty: no reply expected
	}{
		{"mention", post("#public", "alice", "@kam are you there?", 2), "heard alice in 2 hops, drift 0s"},
		{"channel name is normalized", post("PUBLIC", "bob", "@kam", 5), "heard bob in 5 hops, drift 0s"},
		{"own messages are ignored", post("#public", "kam", "@kam ping", 0), ""},
		{"no mention", post("#public", "alice", "anyone there?", 1), ""},
		{"unmonitored channel", post("#mesh-dev", "alice", "@kam hi", 1), ""},
		{"mention at end", post("#public", "alice", "prefix @kam", 1), "heard alice in 1 hops, drift 0s"},
		{"mention before punctuation", post("#public", "alice", "hey @kam!", 1), "heard alice in 1 hops, drift 0s"},
		{"longer name is not a mention", post("#public", "alice", "@kameron nope", 1), ""},
		{"underscore continues the name", post("#public", "alice", "@kam_x nope", 1), ""},
		{"only the first occurrence counts", post("#public", "alice", "@kamino then @kam!", 3), ""},
		{"missing sender", post("#public", "", "@kam", 3), "heard someone in 3 hops, drift 0s"},
		{"drift without radio metadata", drifting, "heard alice in 0 hops, drift 7s"},
		{"radio metadata", withRadio(post("#public", "alice", "@kam report", 1), 29, -64, 3), "heard alice in 1 hops, SNR 7.25 dB, RSSI -64 dBm, drift 3s"},
		{"negative SNR and drift", withRadio(post("#public", "bob", "@kam?", 5), -7, -91, -2), "heard bob in 5 hops, SNR -1.75 dB, RSSI -91 dBm, drift -2s"},
		{"SNR below one dB", withRadio(post("#public", "alice", "@kam", 0), -1, -80, 0), "heard alice in 0 hops, SNR -0.25 dB, RSSI -80 dBm, drift 0s"},
		{"zero SNR", withRadio(post("#public", "alice", "@kam", 0), 0, -80, 0), "heard alice in 0 hops, SNR 0.00 dB, RSSI -80 dBm, drift 0s"},
		{"whole-dB SNR", withRadio(post("#public", "alice", "@kam", 0), 4, -80, 0), "heard alice in 0 hops, SNR 1.00 dB, RSSI -80 dBm, drift 0s"},
	}
	var want []string
	for _, test := range cases {
		runtime.Handle(test.message)
		if test.want != "" {
			want = append(want, test.want)
		}
	}
	waitForReplies(t, sender, len(want))
	time.Sleep(50 * time.Millisecond)
	if got := sender.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("replies differ\n got: %q\nwant: %q", got, want)
	}
}
