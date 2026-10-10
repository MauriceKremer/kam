package room

import (
	"bytes"
	"encoding/binary"
	"log/slog"
	"time"

	"kam.bytecoder.nl/internal/db"
	"kam.bytecoder.nl/internal/kiss"
	"kam.bytecoder.nl/internal/mesh"
)

// Push timing from MyMesh.cpp.
const (
	pushNotifyDelay     = 2000 * time.Millisecond
	syncPushInterval    = 1200 * time.Millisecond
	pushAckTimeoutFlood = 12 * time.Second
	pushTimeoutBase     = 4 * time.Second
	pushTimeoutFactor   = 2 * time.Second
	postSyncDelaySecs   = 6
	maxPushFailures     = 3
)

// tickPush runs the round-robin push engine: one client per pass, one
// unacknowledged post per client.
func (r *roomState) tickPush(now time.Time) {
	if len(r.clients) == 0 || now.Before(r.nextPush) {
		return
	}
	r.expirePending(now)
	if r.nextClient >= len(r.clients) {
		r.nextClient = 0
	}
	c := r.clients[r.nextClient]
	pushed := false
	if !c.hasPending && !c.lastActivity.IsZero() && c.failures < maxPushFailures {
		if post, ok := r.nextPostFor(c, now); ok {
			pushed = r.push(c, post, now)
		}
	}
	r.nextClient = (r.nextClient + 1) % len(r.clients)
	if pushed {
		r.nextPush = now.Add(syncPushInterval)
	} else {
		r.nextPush = now.Add(syncPushInterval / 8) // nothing to send: look at the next client sooner
	}
}

// expirePending turns overdue pushes into failures. A push still waiting in the
// transmit queue (for example deferred by the duty-cycle cap) never reached the
// air, so it is withdrawn without counting against the client.
func (r *roomState) expirePending(now time.Time) {
	for _, c := range r.clients {
		if !c.hasPending || now.Before(c.ackDeadline) {
			continue
		}
		c.hasPending = false
		if r.svc.tx.Cancel(r.pushTag(c)) > 0 {
			continue
		}
		c.failures++
		r.svc.cnt.pushTimeouts.Add(1)
		slog.Info("[Room] push not acknowledged", "room", r.cfg.ID, "key", shortKey(c.pub[:]), "failures", c.failures)
		if c.failures >= maxPushFailures && c.outPathKnown() {
			c.clearOutPath() // rediscover the route on the next exchange (RS-PUSH-6)
		}
	}
}

// nextPostFor returns the oldest cached post the client is owed: newer than its
// cursor, not its own, and at least postSyncDelaySecs old.
func (r *roomState) nextPostFor(c *client, now time.Time) (db.RoomPost, bool) {
	nowSec := uint64(now.Unix())
	for _, p := range r.posts {
		if nowSec >= uint64(p.Timestamp)+postSyncDelaySecs && p.Timestamp > c.syncSince && !bytes.Equal(p.Author, c.pub[:]) {
			return p, true
		}
	}
	return db.RoomPost{}, false
}

// push sends one post to a client and records the ACK it expects.
func (r *roomState) push(c *client, post db.RoomPost, now time.Time) bool {
	if len(post.Author) < 4 {
		return false
	}
	attempt := r.svc.randomBytes(1)[0] & 0x03 // varies per retry so hash and ACK differ
	plain := make([]byte, 0, 9+len(post.Text))
	plain = binary.LittleEndian.AppendUint32(plain, post.Timestamp)
	plain = append(plain, mesh.TxtTypeSignedPlain<<2|attempt)
	plain = append(plain, post.Author[:4]...)
	plain = append(plain, post.Text...)

	payload, err := mesh.PeerPayload(c.pub[0], r.hash, c.secret[:], plain)
	if err != nil {
		slog.Warn("[Room] cannot build push", "room", r.cfg.ID, "error", err)
		return false
	}
	tag := r.pushTag(c)
	var sent bool
	var timeout time.Duration
	if c.outPathKnown() {
		sent = r.sendDirect(c, mesh.PayloadTxtMsg, payload, kiss.TxPriorityPush, 0, tag)
		timeout = pushTimeoutBase + pushTimeoutFactor*time.Duration(int(c.outPathLen&0x3F)+1)
	} else {
		sent = r.sendScopedFlood(mesh.PayloadTxtMsg, payload, kiss.TxPriorityPush, 0, tag)
		timeout = pushAckTimeoutFlood
	}
	if !sent {
		return false
	}
	c.pendingAck = mesh.AckHash(plain, c.pub[:])
	c.hasPending = true
	c.ackDeadline = now.Add(timeout)
	c.pushedPost = post.Timestamp
	r.svc.cnt.pushesSent.Add(1)
	return true
}
