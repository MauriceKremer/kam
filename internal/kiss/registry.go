// internal/kiss/registry.go
package kiss

import (
	"sync"
)

type ResponseRegistry struct {
	mu sync.Mutex
	// Maps a specific response command byte to a channel waiting for its payload
	waiters map[byte]chan []byte
}

// NewResponseRegistry initializes an empty registry
func NewResponseRegistry() *ResponseRegistry {
	return &ResponseRegistry{
		waiters: make(map[byte]chan []byte),
	}
}

// Register prepares the system to receive an asynchronous response.
// It returns a channel that the calling routine can block on.
func (r *ResponseRegistry) Register(cmdByte byte) chan []byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	// We use a buffer size of 1. This is a critical safety feature:
	// It ensures that when the Parser pushes data into this channel,
	// the Parser will NEVER block, even if the calling routine has timed out
	// and abandoned the channel.
	ch := make(chan []byte, 1)
	r.waiters[cmdByte] = ch

	return ch
}

// Deregister cleans up the map after a request is finished or has timed out.
func (r *ResponseRegistry) Deregister(cmdByte byte) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Safely remove the entry from the map to prevent memory leaks
	delete(r.waiters, cmdByte)
}

// Dispatch is called exclusively by your Parser routine when a matching
// hardware response frame arrives.
func (r *ResponseRegistry) Dispatch(cmdByte byte, data []byte) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	ch, exists := r.waiters[cmdByte]
	if !exists {
		// Nobody is listening for this specific packet right now
		return false
	}

	// Hand off the data payload to the waiting routine.
	// Because the channel is buffered, this is an instant, non-blocking operation.
	ch <- data
	return true
}
