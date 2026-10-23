// Package events owns the bounded, metadata-only event ring. The radio and
// room paths record into it through the models.MonitorSink interface;
// internal/node maps its pages into the transport API's response shape. Pages are copied under a short lock and
// encoded after the lock is released, so a slow reader never blocks packet
// recording. Nothing here touches HTTP, SQLite or the modem.
package events

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"kam.bytecoder.nl/internal/models"
)

type Event = models.MonitorEvent

// Page is one read of the ring. It carries no JSON tags: wire shapes belong
// to the transport package, and internal/node maps this value into
// api.EventsResponse.
type Page struct {
	Epoch     string
	FirstSeq  uint64
	LastSeq   uint64
	NextAfter uint64
	Reset     bool
	Events    []Event
}

// Stats is the ring snapshot the API's status endpoint reports.
type Stats struct {
	Epoch    string
	Capacity int
	FirstSeq uint64
	LastSeq  uint64
}

type Ring struct {
	mu       sync.Mutex
	epoch    string
	events   []Event
	next     int
	count    int
	sequence uint64
}

func NewRing(capacity int) *Ring {
	if capacity < 1 {
		panic("event ring capacity must be positive")
	}
	var epoch [16]byte
	if _, err := rand.Read(epoch[:]); err != nil {
		panic(err)
	}
	return &Ring{epoch: hex.EncodeToString(epoch[:]), events: make([]Event, capacity)}
}

func boundedText(value *string) *string {
	if value == nil {
		return nil
	}
	text := strings.ToValidUTF8(*value, "")
	if len(text) > 64 {
		text = text[:64]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
	}
	return &text
}

func (r *Ring) Record(event Event) {
	event.Name = boundedText(event.Name)
	event.Channel = boundedText(event.Channel)
	event.Room = boundedText(event.Room)
	if value := boundedText(&event.Client); value != nil {
		event.Client = *value
	}
	if value := boundedText(&event.Event); value != nil {
		event.Event = *value
	}
	if value := boundedText(&event.Role); value != nil {
		event.Role = *value
	}
	event.DestHash = boundedText(event.DestHash)
	event.AdvertRole = boundedText(event.AdvertRole)
	if event.SNRDB != nil {
		value := *event.SNRDB
		event.SNRDB = &value
	}
	if event.RSSIDBM != nil {
		value := *event.RSSIDBM
		event.RSSIDBM = &value
	}
	if event.AirtimeMS != nil {
		value := *event.AirtimeMS
		event.AirtimeMS = &value
	}
	if event.OK != nil {
		value := *event.OK
		event.OK = &value
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sequence++
	event.Seq = r.sequence
	event.At = event.At.UTC().Truncate(time.Millisecond)
	r.events[r.next] = event
	r.next = (r.next + 1) % len(r.events)
	if r.count < len(r.events) {
		r.count++
	}
}

func (r *Ring) Page(after uint64, epoch string, limit int) (Page, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	response := Page{Epoch: r.epoch, LastSeq: r.sequence, NextAfter: after, Events: []Event{}}
	response.Reset = epoch != "" && epoch != r.epoch
	if !response.Reset && after > r.sequence {
		return response, fmt.Errorf("after is beyond last_seq")
	}
	if r.count == 0 {
		response.NextAfter = 0
		return response, nil
	}
	response.FirstSeq = r.sequence - uint64(r.count) + 1
	if after > 0 && after < response.FirstSeq-1 {
		response.Reset = true
	}
	if response.Reset || after == 0 {
		after = response.FirstSeq - 1
	}
	response.NextAfter = after
	start := int(after + 1 - response.FirstSeq)
	oldest := (r.next - r.count + len(r.events)) % len(r.events)
	for index := start; index < r.count && len(response.Events) < limit; index++ {
		event := r.events[(oldest+index)%len(r.events)]
		response.Events = append(response.Events, event)
		response.NextAfter = event.Seq
	}
	return response, nil
}

func (r *Ring) Stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	var first uint64
	if r.count > 0 {
		first = r.sequence - uint64(r.count) + 1
	}
	return Stats{Epoch: r.epoch, Capacity: len(r.events), FirstSeq: first, LastSeq: r.sequence}
}
