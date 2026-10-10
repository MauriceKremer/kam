package room

import (
	"encoding/hex"
	"log/slog"
	"time"

	"kam.bytecoder.nl/internal/kiss"
	"kam.bytecoder.nl/internal/mesh"
)

// submit encodes a packet and queues it for transmission. It never blocks.
func (s *Service) submit(pkt mesh.Packet, priority kiss.TxPriority, delay time.Duration, tag string) bool {
	raw, err := pkt.Encode()
	if err != nil {
		slog.Warn("[Room] cannot encode packet", "error", err)
		return false
	}
	if err := s.tx.Submit(kiss.TxRequest{Raw: raw, Priority: priority, Delay: delay, Tag: tag}); err != nil {
		s.cnt.txRejected.Add(1)
		slog.Warn("[Room] transmit queue refused a packet", "error", err, "priority", int(priority))
		return false
	}
	return true
}

// replyScope picks the scope for a flood reply (RoutingPolicy.h
// chooseReplyScope): the request's own region when it matches a configured
// one, unscoped when the request was an unscoped flood, otherwise the default
// scope, otherwise unscoped.
func (s *Service) replyScope(req mesh.Packet) *mesh.TransportKey {
	switch req.RouteType() {
	case mesh.RouteTransportFlood:
		for _, rg := range s.regions {
			if rg.key.Code(req.PayloadType(), req.Payload) == req.TransportCodes[0] {
				key := rg.key
				return &key
			}
		}
	case mesh.RouteFlood:
		return nil
	}
	return s.defaultScope
}

// sendDirect sends along the client's stored route.
func (r *roomState) sendDirect(c *client, payloadType byte, payload []byte, priority kiss.TxPriority, delay time.Duration, tag string) bool {
	pkt, err := mesh.NewDirectPacket(payloadType, payload, c.outPathLen, c.outPath)
	if err != nil {
		slog.Warn("[Room] cannot build direct packet", "room", r.cfg.ID, "error", err)
		return false
	}
	return r.svc.submit(pkt, priority, delay, tag)
}

// sendFloodReply floods a reply using the request's hash size and scope.
func (r *roomState) sendFloodReply(req mesh.Packet, payloadType byte, payload []byte, delay time.Duration) bool {
	pkt, err := mesh.NewFloodPacket(payloadType, payload, req.HashSize(), r.svc.replyScope(req))
	if err != nil {
		slog.Warn("[Room] cannot build flood reply", "room", r.cfg.ID, "error", err)
		return false
	}
	return r.svc.submit(pkt, kiss.TxPriorityFloodReply, delay, "")
}

// sendReply answers a request: direct when the client's route is known, else
// flooded.
func (r *roomState) sendReply(c *client, req mesh.Packet, payloadType byte, payload []byte, delay time.Duration) bool {
	if c.outPathKnown() {
		return r.sendDirect(c, payloadType, payload, kiss.TxPriorityReply, delay, "")
	}
	return r.sendFloodReply(req, payloadType, payload, delay)
}

// sendScopedFlood floods a packet the room originates (pushes, adverts) in the
// default scope with the configured hash size.
func (r *roomState) sendScopedFlood(payloadType byte, payload []byte, priority kiss.TxPriority, delay time.Duration, tag string) bool {
	pkt, err := mesh.NewFloodPacket(payloadType, payload, r.svc.mesh.PathHashSize, r.svc.defaultScope)
	if err != nil {
		slog.Warn("[Room] cannot build flood packet", "room", r.cfg.ID, "error", err)
		return false
	}
	return r.svc.submit(pkt, priority, delay, tag)
}

// pushTag identifies the push queued for one client so it can be cancelled.
func (r *roomState) pushTag(c *client) string {
	return "push:" + r.cfg.ID + ":" + hex.EncodeToString(c.pub[:8])
}
