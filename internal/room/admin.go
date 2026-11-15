// Management calls from the local API (RS-OPS-2): forget a client, delete a
// post, send an advert now. Each one runs on the worker goroutine, which owns
// room state, through adminIn.

package room

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	"kam.bytecoder.nl/internal/mesh"
)

// Management verdicts the REST layer maps to a status code.
var (
	ErrRoomNotFound   = errors.New("room not found")
	ErrClientNotFound = errors.New("room client not found")
	ErrPostNotFound   = errors.New("room post not found")
)

// adminRequest is one management mutation; apply runs on the worker goroutine
// and reply has capacity one so the worker never blocks on a gone caller.
type adminRequest struct {
	roomID string
	apply  func(*roomState) error
	reply  chan error
}

func (s *Service) admin(ctx context.Context, roomID string, apply func(*roomState) error) error {
	request := adminRequest{roomID: roomID, apply: apply, reply: make(chan error, 1)}
	select {
	case <-s.stop:
		return errors.New("room service is stopped")
	default:
	}
	select {
	case s.adminIn <- request:
	default:
		return errors.New("management queue is full")
	}
	select {
	case err := <-request.reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-s.stop:
		return errors.New("room service is stopped")
	}
}

func (s *Service) applyAdmin(request adminRequest) error {
	for _, r := range s.rooms {
		if r.cfg.ID == request.roomID {
			return request.apply(r)
		}
	}
	return ErrRoomNotFound
}

// RemoveClient forgets a client's session: its stored record is deleted, then
// it leaves the room's table and its pending pushes are cancelled. A failed
// delete leaves the session intact and is reported. It is not a ban; the
// client can log in again.
func (s *Service) RemoveClient(ctx context.Context, roomID, publicKey string) error {
	raw, err := hex.DecodeString(publicKey)
	if err != nil || len(raw) != mesh.PubKeySize {
		return fmt.Errorf("public key must be %d hex characters", mesh.PubKeySize*2)
	}
	var pub [32]byte
	copy(pub[:], raw)
	return s.admin(ctx, roomID, func(r *roomState) error {
		c := r.byKey[pub]
		if c == nil {
			return ErrClientNotFound
		}
		storeCtx, cancel := storeContext()
		defer cancel()
		if err := r.svc.store.DeleteClient(storeCtx, r.cfg.ID, c.pub[:]); err != nil {
			return err
		}
		role := roleName(c.role)
		r.dropClient(c)
		r.svc.recordRoomEvent(r.cfg.ID, "client_removed", publicKey, role, r.svc.now())
		return nil
	})
}

// DeletePost removes one post from the store and from the cached backlog, so
// it is neither served nor pushed again.
func (s *Service) DeletePost(ctx context.Context, roomID string, postID uint32) error {
	return s.admin(ctx, roomID, func(r *roomState) error {
		storeCtx, cancel := storeContext()
		defer cancel()
		found, err := r.svc.store.DeletePost(storeCtx, r.cfg.ID, postID)
		if err != nil {
			return err
		}
		if !found {
			return ErrPostNotFound
		}
		for i, post := range r.posts {
			if post.Timestamp == postID {
				r.posts = append(r.posts[:i], r.posts[i+1:]...)
				break
			}
		}
		return nil
	})
}

// ForceAdvert queues a flood advert now instead of waiting for the interval.
func (s *Service) ForceAdvert(ctx context.Context, roomID string) error {
	return s.admin(ctx, roomID, func(r *roomState) error {
		return r.sendAdvert(r.svc.now(), true)
	})
}
