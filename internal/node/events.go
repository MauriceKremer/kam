package node

import (
	"kam.bytecoder.nl/internal/api"
	"kam.bytecoder.nl/internal/events"
)

// apiEvents adapts the ring to the transport API's EventsSource seam, mapping
// the ring's Pages into the API's wire shape.
type apiEvents struct{ ring *events.Ring }

func (s apiEvents) Page(after uint64, epoch string, limit int) (api.EventsResponse, error) {
	page, err := s.ring.Page(after, epoch, limit)
	if err != nil {
		return api.EventsResponse{}, err
	}
	return api.EventsResponse{Epoch: page.Epoch, FirstSeq: page.FirstSeq, LastSeq: page.LastSeq, NextAfter: page.NextAfter, Reset: page.Reset, Events: page.Events}, nil
}

func (s apiEvents) Stats() api.RingStats {
	stats := s.ring.Stats()
	return api.RingStats{Epoch: stats.Epoch, Capacity: stats.Capacity, FirstSeq: stats.FirstSeq, LastSeq: stats.LastSeq}
}
