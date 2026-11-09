package room

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"kam.bytecoder.nl/internal/db"
	"kam.bytecoder.nl/internal/kiss"
	"kam.bytecoder.nl/internal/mesh"
	"kam.bytecoder.nl/internal/models"
)

// Roles are the low two bits of a client's permissions.
const (
	RoleGuest     uint8 = 0
	RoleReadOnly  uint8 = 1
	RoleReadWrite uint8 = 2
	RoleAdmin     uint8 = 3
)

const (
	reqKeepAlive = 0x02

	// Delays and limits from MyMesh.cpp / MyMesh.h.
	serverResponseDelay = 300 * time.Millisecond
	txtAckDelay         = 200 * time.Millisecond
	maxPostTextLen      = 150
	firmwareVerLevel    = 1

	clientFlushInterval = 5 * time.Second
	pruneInterval       = 10 * time.Minute
	startupAdvertGap    = 2 * time.Second
)

// client is one logged-in (or remembered) peer of a room.
type client struct {
	pub    [32]byte
	secret [32]byte
	role   uint8

	// lastTimestamp is the newest timestamp seen on the client's own clock; it
	// provides replay protection.
	lastTimestamp uint32
	// lastActivity is the last valid request time; it survives restarts for
	// monitoring and eviction. active is current-run contact and gates pushes.
	lastActivity time.Time
	active       bool
	// syncSince is the cursor: posts newer than it are still owed.
	syncSince uint32

	pendingAck  [4]byte
	hasPending  bool
	ackDeadline time.Time
	pushedPost  uint32
	failures    int

	outPathLen byte
	outPath    []byte

	dirty bool
}

func (c *client) outPathKnown() bool { return c.outPathLen != mesh.OutPathUnknown }

func roleName(role uint8) string {
	switch role {
	case RoleAdmin:
		return "admin"
	case RoleReadWrite:
		return "read_write"
	case RoleReadOnly:
		return "read_only"
	default:
		return "guest"
	}
}

func (c *client) clearOutPath() {
	c.outPathLen = mesh.OutPathUnknown
	c.outPath = nil
	c.dirty = true
}

func (c *client) record() db.RoomClient {
	return db.RoomClient{
		PublicKey: append([]byte(nil), c.pub[:]...), Role: c.role, SyncSince: c.syncSince,
		LastTimestamp: c.lastTimestamp, OutPathLen: c.outPathLen, OutPath: append([]byte(nil), c.outPath...),
		LastHeardAt: optionalTime(c.lastActivity),
	}
}

func optionalTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	stamp := value.UTC()
	return &stamp
}

// roomState is everything one room owns. Only the service worker touches it.
type roomState struct {
	svc  *Service
	cfg  models.RoomConfig
	id   *mesh.Identity
	pub  [32]byte
	hash byte

	clients    []*client
	byKey      map[[32]byte]*client
	posts      []db.RoomPost // newest BacklogPosts posts, oldest first
	cnt        counters
	nextClient int
	nextPush   time.Time

	startupAt       time.Time
	startupSent     bool
	nextLocalAdvert time.Time
	nextFloodAdvert time.Time

	nextFlush time.Time
	nextPrune time.Time
}

func (s *Service) loadRoom(spec Spec, index int, now time.Time) (*roomState, error) {
	cfg := spec.Config
	r := &roomState{
		svc: s, cfg: cfg, id: spec.Identity,
		pub: spec.Identity.PublicKey(), hash: spec.Identity.Hash(),
		byKey:     make(map[[32]byte]*client),
		startupAt: now.Add(time.Duration(index) * startupAdvertGap),
		nextFlush: now.Add(clientFlushInterval),
		nextPrune: now,
	}

	ctx, cancel := storeContext()
	defer cancel()
	changedFrom, err := s.store.RegisterRoom(ctx, cfg.ID, r.pub[:])
	if err != nil {
		return nil, err
	}
	if changedFrom != nil {
		slog.Warn("[Room] identity differs from the one stored for this room; clients know the old address", "room", cfg.ID, "old", shortKey(changedFrom), "new", shortKey(r.pub[:]))
	}
	posts, err := s.store.LatestPosts(ctx, cfg.ID, cfg.BacklogPosts)
	if err != nil {
		return nil, err
	}
	r.posts = posts
	if n := len(posts); n > 0 && posts[n-1].Timestamp > uint32(now.Unix()) {
		slog.Warn("[Room] host clock is earlier than the newest stored post; clients may not receive new posts until the clock catches up", "room", cfg.ID)
	}
	stored, err := s.store.LoadClients(ctx, cfg.ID)
	if err != nil {
		return nil, err
	}
	for _, rc := range stored {
		if len(r.clients) >= cfg.MaxClients {
			break
		}
		if c := r.restoreClient(rc); c != nil {
			r.clients = append(r.clients, c)
			r.byKey[c.pub] = c
		}
	}
	s.clients.Add(int64(len(r.clients)))
	slog.Info("[Room] room ready", "room", cfg.ID, "name", cfg.Name, "key", shortKey(r.pub[:]), "hash", r.hash, "posts_cached", len(r.posts), "clients", len(r.clients))
	return r, nil
}

// restoreClient rebuilds a client from storage, recomputing its shared secret.
func (r *roomState) restoreClient(rc db.RoomClient) *client {
	if len(rc.PublicKey) != mesh.PubKeySize {
		return nil
	}
	secret, err := r.id.SharedSecret(rc.PublicKey)
	if err != nil {
		slog.Warn("[Room] dropping stored client with an unusable key", "room", r.cfg.ID, "key", shortKey(rc.PublicKey))
		return nil
	}
	c := &client{secret: secret, role: rc.Role & 0x03, syncSince: rc.SyncSince, lastTimestamp: rc.LastTimestamp, outPathLen: mesh.OutPathUnknown}
	if rc.LastHeardAt != nil {
		c.lastActivity = *rc.LastHeardAt
	}
	copy(c.pub[:], rc.PublicKey)
	if mesh.ValidPathLen(rc.OutPathLen) && len(rc.OutPath) == int(rc.OutPathLen&0x3F)*(int(rc.OutPathLen>>6)+1) {
		c.outPathLen = rc.OutPathLen
		c.outPath = append([]byte(nil), rc.OutPath...)
	}
	return c
}

func (r *roomState) tick(now time.Time) {
	r.tickAdverts(now)
	r.tickPush(now)
	r.flushClients(now, false)
	r.tickPrune(now)
}

// flushClients writes changed client sessions. Posts do not go through here:
// they are committed before they are acknowledged.
func (r *roomState) flushClients(now time.Time, force bool) {
	if !force && now.Before(r.nextFlush) {
		return
	}
	r.nextFlush = now.Add(clientFlushInterval)
	var batch []db.RoomClient
	var flagged []*client
	for _, c := range r.clients {
		if c.dirty {
			batch = append(batch, c.record())
			flagged = append(flagged, c)
		}
	}
	if len(batch) == 0 {
		return
	}
	ctx, cancel := storeContext()
	defer cancel()
	if err := r.svc.store.SaveClients(ctx, r.cfg.ID, batch); err != nil {
		slog.Error("[Room] could not save client sessions; will retry", "room", r.cfg.ID, "error", err)
		return
	}
	for _, c := range flagged {
		c.dirty = false
	}
}

func (r *roomState) tickPrune(now time.Time) {
	if now.Before(r.nextPrune) {
		return
	}
	r.nextPrune = now.Add(pruneInterval)
	if r.cfg.MaxPosts == 0 && r.cfg.MaxAgeDays == 0 {
		return
	}
	var olderThan uint32
	if r.cfg.MaxAgeDays > 0 {
		cutoff := now.Add(-time.Duration(r.cfg.MaxAgeDays) * 24 * time.Hour).Unix()
		if cutoff > 0 {
			olderThan = uint32(cutoff)
		}
	}
	ctx, cancel := storeContext()
	defer cancel()
	deleted, err := r.svc.store.PrunePosts(ctx, r.cfg.ID, r.cfg.MaxPosts, olderThan)
	if err != nil {
		slog.Error("[Room] could not prune posts", "room", r.cfg.ID, "error", err)
		return
	}
	if deleted > 0 {
		slog.Info("[Room] pruned old posts", "room", r.cfg.ID, "deleted", deleted)
	}
}

// equalSecret compares two secrets in constant time regardless of length.
func equalSecret(a, b string) bool {
	ha, hb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

// authorize maps a login password to a role (MyMesh::onAnonDataRecv with the
// open-room rule of docs/FeatureRoomServer.md D10). Every comparison runs
// before the result is used so timing does not reveal which one matched.
func (r *roomState) authorize(password string) (role uint8, ok bool) {
	isAdmin := equalSecret(password, r.cfg.AdminPassword)
	isGuest := false
	if r.cfg.GuestPassword != "" {
		isGuest = equalSecret(password, r.cfg.GuestPassword)
	} else if r.cfg.OpenRoom {
		isGuest = password == ""
	}
	switch {
	case isAdmin:
		return RoleAdmin, true
	case isGuest:
		return RoleReadWrite, true
	case r.cfg.AllowReadOnly:
		return RoleGuest, true
	}
	return 0, false
}

// putClient returns the client for key, creating it when absent. When the table
// is full the least recently active non-admin is evicted; if only admins remain
// it returns nil.
func (r *roomState) putClient(pub, secret [32]byte) *client {
	if c := r.byKey[pub]; c != nil {
		return c
	}
	if len(r.clients) >= r.cfg.MaxClients {
		victim := r.evictionCandidate()
		if victim == nil {
			return nil
		}
		slog.Info("[Room] client table full; evicting the least recently active client", "room", r.cfg.ID, "key", shortKey(victim.pub[:]))
		r.removeClient(victim)
	}
	c := &client{pub: pub, secret: secret, outPathLen: mesh.OutPathUnknown}
	r.clients = append(r.clients, c)
	r.byKey[pub] = c
	r.svc.clients.Add(1)
	return c
}

func (r *roomState) evictionCandidate() *client {
	var victim *client
	for _, c := range r.clients {
		if c.role == RoleAdmin {
			continue
		}
		if victim == nil || c.lastActivity.Before(victim.lastActivity) {
			victim = c
		}
	}
	return victim
}

func (r *roomState) removeClient(c *client) {
	r.detachClient(c)
	ctx, cancel := storeContext()
	defer cancel()
	if err := r.svc.store.DeleteClient(ctx, r.cfg.ID, c.pub[:]); err != nil {
		slog.Error("[Room] could not delete evicted client", "room", r.cfg.ID, "error", err)
	}
}

// detachClient drops a client's runtime state: its table entry, so no later
// push, ACK match or batch write includes it, and any push still queued for it.
func (r *roomState) detachClient(c *client) {
	for i, other := range r.clients {
		if other == c {
			r.clients = append(r.clients[:i], r.clients[i+1:]...)
			if r.nextClient > i {
				r.nextClient--
			}
			break
		}
	}
	delete(r.byKey, c.pub)
	r.svc.clients.Add(-1)
	r.svc.tx.Cancel(r.pushTag(c))
}

// forgetClient runs on the worker for RemoveClient. The stored record goes
// first: a failed delete leaves the session untouched, and once the record is
// gone the client leaves the table before the next batch write can see it.
func (s *Service) forgetClient(roomID string, key [32]byte, now time.Time) error {
	r := s.roomByID(roomID)
	if r == nil {
		return ErrUnknownRoom
	}
	c := r.byKey[key]
	if c == nil {
		return ErrUnknownClient
	}
	ctx, cancel := storeContext()
	defer cancel()
	if err := s.store.DeleteClient(ctx, roomID, key[:]); err != nil {
		slog.Error("[Room] could not delete client; its session is kept", "room", roomID, "key", shortKey(key[:]), "error", err)
		return fmt.Errorf("delete room client: %w", err)
	}
	r.detachClient(c)
	slog.Info("[Room] client removed", "room", roomID, "key", shortKey(key[:]), "role", roleName(c.role))
	s.recordRoomEvent(roomID, "client_removed", hex.EncodeToString(key[:]), roleName(c.role), now)
	return nil
}

// handleAck matches an ACK against every client's outstanding push.
func (s *Service) handleAck(data []byte) bool {
	if len(data) < 4 {
		return false
	}
	for _, r := range s.rooms {
		for _, c := range r.clients {
			if c.hasPending && bytes.Equal(data[:4], c.pendingAck[:]) {
				c.hasPending = false
				c.failures = 0
				c.syncSince = c.pushedPost
				c.dirty = true
				s.cnt.pushesAcked.Add(1)
				r.cnt.pushesAcked.Add(1)
				s.recordRoomEvent(r.cfg.ID, "push_acked", hex.EncodeToString(c.pub[:]), roleName(c.role), s.now())
				return true
			}
		}
	}
	return false
}

// handleAnon processes an ANON_REQ, the login request from a client the room
// does not know yet.
func (s *Service) handleAnon(pkt mesh.Packet, now time.Time) {
	p := pkt.Payload
	if len(p) <= 1+mesh.PubKeySize+mesh.CipherMACSize {
		return
	}
	var matching []*roomState
	for _, r := range s.rooms {
		if r.hash == p[0] {
			matching = append(matching, r)
		}
	}
	if len(matching) == 0 {
		return
	}
	var pub [32]byte
	copy(pub[:], p[1:1+mesh.PubKeySize])
	if !s.limiter.allow(pub, now) {
		s.cnt.loginsRateLimited.Add(1)
		if len(matching) > 0 {
			matching[0].cnt.loginsRateLimited.Add(1)
			s.recordRoomEvent(matching[0].cfg.ID, "login_rate_limited", hex.EncodeToString(pub[:]), "", now)
		}
		slog.Info("[Room] login attempt rate limited", "key", shortKey(pub[:]))
		return
	}
	macAndData := p[1+mesh.PubKeySize:]
	for _, r := range matching {
		secret, err := r.id.SharedSecret(pub[:])
		if err != nil {
			continue
		}
		plain, ok := mesh.MACThenDecrypt(secret[:], macAndData)
		if !ok {
			continue
		}
		r.onLogin(pkt, pub, secret, plain, now)
		return
	}
}

// handlePeer processes REQ, TXT_MSG and PATH packets from known clients. With
// one-byte hashes several clients can share src_hash, so every candidate is
// tried until a MAC verifies.
func (s *Service) handlePeer(pkt mesh.Packet, now time.Time) {
	p := pkt.Payload
	if len(p) <= 2+mesh.CipherMACSize {
		return
	}
	dest, src, macAndData := p[0], p[1], p[2:]
	for _, r := range s.rooms {
		if r.hash != dest {
			continue
		}
		for _, c := range r.clients {
			if c.pub[0] != src {
				continue
			}
			plain, ok := mesh.MACThenDecrypt(c.secret[:], macAndData)
			if !ok {
				continue
			}
			switch pkt.PayloadType() {
			case mesh.PayloadPath:
				r.onPath(c, plain, now)
			case mesh.PayloadTxtMsg:
				r.onText(c, pkt, plain, now)
			case mesh.PayloadReq:
				r.onRequest(c, pkt, plain, now)
			}
			return
		}
	}
}

func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// onLogin handles a decrypted login: timestamp(4) | sync_since(4) | password.
func (r *roomState) onLogin(pkt mesh.Packet, pub, secret [32]byte, plain []byte, now time.Time) {
	s := r.svc
	if len(plain) < 9 {
		return
	}
	sentAt := binary.LittleEndian.Uint32(plain[0:4])
	since := binary.LittleEndian.Uint32(plain[4:8])
	password := cString(plain[8:])

	var c *client
	if password == "" {
		// A blank password from a known client resumes the session; nothing is
		// re-checked and the cursor, pending ACK and failures stay as they are.
		c = r.byKey[pub]
		if c != nil {
			c.lastActivity = now
			c.active = true
		}
	}
	if c == nil {
		role, ok := r.authorize(password)
		if !ok {
			s.cnt.loginsFailed.Add(1)
			r.cnt.loginsFailed.Add(1)
			s.recordRoomEvent(r.cfg.ID, "login_failed", hex.EncodeToString(pub[:]), "", now)
			slog.Info("[Room] login refused", "room", r.cfg.ID, "key", shortKey(pub[:]))
			return
		}
		existing := r.byKey[pub]
		if (existing != nil && sentAt <= existing.lastTimestamp) || (existing == nil && sentAt == 0) {
			s.cnt.loginsReplayed.Add(1)
			slog.Info("[Room] login dropped as a replay", "room", r.cfg.ID, "key", shortKey(pub[:]))
			return
		}
		c = r.putClient(pub, secret)
		if c == nil {
			s.cnt.loginsFailed.Add(1)
			r.cnt.loginsFailed.Add(1)
			s.recordRoomEvent(r.cfg.ID, "login_failed", hex.EncodeToString(pub[:]), "", now)
			slog.Warn("[Room] login refused: client table is full of admins", "room", r.cfg.ID)
			return
		}
		c.lastTimestamp = sentAt
		c.syncSince = since
		c.hasPending = false
		s.tx.Cancel(r.pushTag(c))
		c.failures = 0
		c.lastActivity = now
		c.active = true
		c.role = role
		c.secret = secret
	}
	if pkt.IsFlood() {
		c.clearOutPath() // the route is rediscovered from the PATH the client returns
	}
	c.dirty = true
	r.nextPush = now.Add(pushNotifyDelay) // let the response arrive before posts
	s.cnt.loginsOK.Add(1)
	r.cnt.loginsOK.Add(1)
	s.recordRoomEvent(r.cfg.ID, "login_ok", hex.EncodeToString(c.pub[:]), roleName(c.role), now)
	slog.Info("[Room] login accepted", "room", r.cfg.ID, "key", shortKey(pub[:]), "role", c.role)

	response := r.loginResponse(c, now)
	if pkt.IsFlood() {
		payload, err := mesh.PathReturnPayload(c.pub[0], r.hash, c.secret[:], pkt.PathLen, pkt.Path, mesh.PayloadResponse, response, s.random)
		if err != nil {
			slog.Warn("[Room] cannot build login path return", "room", r.cfg.ID, "error", err)
			return
		}
		r.sendFloodReply(pkt, mesh.PayloadPath, payload, serverResponseDelay)
		return
	}
	payload, err := mesh.PeerPayload(c.pub[0], r.hash, c.secret[:], response)
	if err != nil {
		slog.Warn("[Room] cannot build login response", "room", r.cfg.ID, "error", err)
		return
	}
	r.sendReply(c, pkt, mesh.PayloadResponse, payload, serverResponseDelay)
}

// loginResponse builds the 13-byte RESPONSE plaintext.
func (r *roomState) loginResponse(c *client, now time.Time) []byte {
	response := make([]byte, 13)
	binary.LittleEndian.PutUint32(response[0:4], r.svc.uniqueNow(now))
	response[4] = 0 // RESP_SERVER_LOGIN_OK
	response[5] = 0 // legacy keep-alive interval
	switch {
	case c.role == RoleAdmin:
		response[6] = 1
	case c.role == RoleGuest:
		response[6] = 2
	}
	response[7] = c.role
	copy(response[8:12], r.svc.randomBytes(4))
	response[12] = firmwareVerLevel
	return response
}

// onPath records the route a client returned to us. No reciprocal path is
// sent, matching the reference.
func (r *roomState) onPath(c *client, plain []byte, now time.Time) {
	pathLen, path, extraType, extra, err := mesh.ParsePathReturn(plain)
	if err != nil {
		return
	}
	c.outPathLen = pathLen
	c.outPath = path
	c.lastActivity = now
	c.active = true
	c.dirty = true
	if extraType == mesh.PayloadAck && len(extra) >= 4 {
		r.svc.handleAck(extra[:4])
	}
}

// onText handles a TXT_MSG from a client: a post, or a CLI command that is not
// supported (docs/FeatureRoomServer.md D5).
func (r *roomState) onText(c *client, pkt mesh.Packet, plain []byte, now time.Time) {
	s := r.svc
	if len(plain) <= 5 {
		return
	}
	sentAt := binary.LittleEndian.Uint32(plain[0:4])
	kind := plain[4] >> 2
	if kind != mesh.TxtTypePlain && kind != mesh.TxtTypeCLIData {
		slog.Debug("[Room] ignoring unsupported text type", "room", r.cfg.ID, "type", kind)
		return
	}
	if sentAt < c.lastTimestamp {
		return // replay; an equal timestamp is a retry and is acknowledged again
	}
	isRetry := sentAt == c.lastTimestamp
	c.lastActivity = now
	c.active = true
	c.dirty = true
	c.failures = 0

	text := cString(plain[5:])
	if kind == mesh.TxtTypeCLIData || c.role < RoleReadWrite {
		c.lastTimestamp = sentAt
		c.dirty = true
		if kind == mesh.TxtTypePlain {
			s.cnt.postsRejected.Add(1)
			r.cnt.postsRejected.Add(1)
			s.recordRoomEvent(r.cfg.ID, "post_rejected", hex.EncodeToString(c.pub[:]), roleName(c.role), now)
		}
		return // no reply and no ACK
	}

	ack := mesh.AckHash(plain[:5+len(text)], c.pub[:])
	if !isRetry && text != "" {
		if err := r.addPost(c, text, sentAt, now); err != nil {
			s.cnt.postsRejected.Add(1)
			r.cnt.postsRejected.Add(1)
			s.recordRoomEvent(r.cfg.ID, "post_rejected", hex.EncodeToString(c.pub[:]), roleName(c.role), now)
			slog.Error("[Room] post not stored; no ACK sent so the client retries", "room", r.cfg.ID, "key", shortKey(c.pub[:]), "error", err)
			return
		}
	} else {
		c.lastTimestamp = sentAt
		c.dirty = true
	}
	r.sendReply(c, pkt, mesh.PayloadAck, ack[:], txtAckDelay)
}

// addPost stores a post and only then updates the client's timestamp, so a
// failed write leaves the client free to retry.
func (r *roomState) addPost(c *client, text string, clientTimestamp uint32, now time.Time) error {
	if err := r.storePost(c.pub[:], text, c.pub[:], clientTimestamp, now, hex.EncodeToString(c.pub[:]), roleName(c.role)); err != nil {
		return err
	}
	c.lastTimestamp = clientTimestamp
	return nil
}

// storePost persists one post and, only on success, adds it to the in-memory
// backlog that clients are served from. A nil clientKey skips the client
// timestamp update, which is what a room-authored post needs.
func (r *roomState) storePost(author []byte, text string, clientKey []byte, clientTimestamp uint32, now time.Time, client, role string) error {
	s := r.svc
	text = mesh.TruncateUTF8(text, maxPostTextLen)
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		post := db.RoomPost{Timestamp: s.uniqueNow(now), Author: append([]byte(nil), author...), Text: text, CreatedAt: now.UTC().Truncate(time.Second)}
		ctx, cancel := storeContext()
		err := s.store.AppendPost(ctx, r.cfg.ID, post, clientKey, clientTimestamp)
		cancel()
		if err == nil {
			r.posts = append(r.posts, post)
			if over := len(r.posts) - r.cfg.BacklogPosts; over > 0 {
				r.posts = append([]db.RoomPost(nil), r.posts[over:]...)
			}
			r.nextPush = now.Add(pushNotifyDelay)
			s.cnt.postsAccepted.Add(1)
			r.cnt.postsAccepted.Add(1)
			s.recordRoomEvent(r.cfg.ID, "post", client, role, now)
			return nil
		}
		lastErr = err
		if !errors.Is(err, db.ErrRoomPostTimestampTaken) {
			break
		}
		// The clock stepped back across a restart onto a stored post; the next
		// unique timestamp is one higher.
	}
	return lastErr
}

// addSystemPost runs on the worker and stores a post authored by the room
// itself, so scripts and adverts reach clients through the normal backlog and
// push paths.
func (s *Service) addSystemPost(roomID, text string, now time.Time) error {
	room := s.roomByID(roomID)
	if room == nil {
		return ErrUnknownRoom
	}
	if err := room.storePost(room.pub[:], text, nil, 0, now, "", "system"); err != nil {
		slog.Error("[Room] system post failed", "room", roomID, "error", err)
		return err
	}
	return nil
}

// onRequest handles a REQ from a client. Keep-alive is supported; status,
// telemetry and access-list requests get no reply.
func (r *roomState) onRequest(c *client, pkt mesh.Packet, plain []byte, now time.Time) {
	s := r.svc
	if len(plain) < 9 {
		return
	}
	sentAt := binary.LittleEndian.Uint32(plain[0:4])
	if sentAt < c.lastTimestamp {
		return
	}
	c.lastTimestamp = sentAt
	c.lastActivity = now
	c.active = true
	c.failures = 0
	c.dirty = true

	if plain[4] != reqKeepAlive {
		s.cnt.unsupportedRequests.Add(1)
		slog.Debug("[Room] request type not supported", "room", r.cfg.ID, "type", plain[4])
		return
	}
	if !pkt.IsDirect() {
		return // keep-alive is honoured on direct routes only
	}
	if forceSince := binary.LittleEndian.Uint32(plain[5:9]); forceSince > 0 {
		c.syncSince = forceSince
	}
	c.hasPending = false
	s.tx.Cancel(r.pushTag(c))
	if !c.outPathKnown() {
		return // keep-alive replies go direct only
	}
	ack := mesh.AckHash(plain[:9], c.pub[:])
	payload := append(ack[:], r.unsyncedCount(c))
	r.sendDirect(c, mesh.PayloadAck, payload, kiss.TxPriorityReply, serverResponseDelay, "")
}

// unsyncedCount is the number of cached posts the client still has to receive.
func (r *roomState) unsyncedCount(c *client) byte {
	n := 0
	for _, p := range r.posts {
		if p.Timestamp > c.syncSince && !bytes.Equal(p.Author, c.pub[:]) {
			n++
		}
	}
	if n > 255 {
		n = 255
	}
	return byte(n)
}
