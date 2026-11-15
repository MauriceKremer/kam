// Management calls from the local API (RS-OPS-2): delete a post or send an
// advert now. Each runs on the worker goroutine through adminIn.

package room

import (
	"context"
	"errors"
)

// Management verdicts the REST layer maps to a status code.
var (
	ErrRoomNotFound = errors.New("room not found")
	ErrPostNotFound = errors.New("room post not found")
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
