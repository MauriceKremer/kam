package events

import (
	"testing"
	"time"
)

func TestRingOwnsCapturedValues(t *testing.T) {
	ring := NewRing(256)
	name := "before"
	snr := 8.5
	ring.Record(Event{At: time.Now(), Name: &name, SNRDB: &snr})
	name = "after"
	snr = 0
	page, _ := ring.Page(0, "", 100)
	if *page.Events[0].Name != "before" || *page.Events[0].SNRDB != 8.5 {
		t.Fatal("captured values mutated")
	}
}

func TestEmptyRingPage(t *testing.T) {
	ring := NewRing(256)
	page, err := ring.Page(0, "", 1000)
	if err != nil || page.FirstSeq != 0 || page.LastSeq != 0 || page.NextAfter != 0 || page.Events == nil {
		t.Fatalf("empty page: %+v %v", page, err)
	}
	if page, _ = ring.Page(100, "old-epoch", 1000); !page.Reset || page.NextAfter != 0 {
		t.Fatalf("empty reset: %+v", page)
	}
}

func TestRingStats(t *testing.T) {
	ring := NewRing(4)
	stats := ring.Stats()
	if stats.Capacity != 4 || stats.FirstSeq != 0 || stats.LastSeq != 0 || stats.Epoch == "" {
		t.Fatalf("empty stats: %+v", stats)
	}
	for range 6 {
		ring.Record(Event{Kind: "rx", At: time.Now()})
	}
	stats = ring.Stats()
	if stats.Capacity != 4 || stats.FirstSeq != 3 || stats.LastSeq != 6 {
		t.Fatalf("wrapped stats: %+v", stats)
	}
	page, _ := ring.Page(0, "", 100)
	if page.Epoch != stats.Epoch || page.FirstSeq != stats.FirstSeq || page.LastSeq != stats.LastSeq {
		t.Fatal("stats disagree with the page")
	}
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
				ring.Page(0, "", 1000)
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
