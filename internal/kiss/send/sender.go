package send

import (
	"context"
	"fmt"

	"kam.bytecoder.nl/internal/kiss"
)

// TxPusher is the minimal interface needed to enqueue KISS frames.
type TxPusher interface {
	TxChan() chan<- []byte
}

// Sender pushes KISS CMD_DATA frames for group texts into a TxPusher.
type Sender struct {
	manager TxPusher
	builder *Builder
}

// NewSender creates a Sender for the given TxPusher and local node name.
func NewSender(manager TxPusher, nodeName string) *Sender {
	return &Sender{
		manager: manager,
		builder: NewBuilder(nodeName),
	}
}

// Send builds a group text packet for channelName and enqueues it for TX.
func (s *Sender) Send(ctx context.Context, channelName, body string) error {
	raw, err := s.builder.Build(channelName, body)
	if err != nil {
		return fmt.Errorf("build group text: %w", err)
	}

	frame := append([]byte{kiss.CMD_DATA}, raw...)

	select {
	case s.manager.TxChan() <- frame:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// NewForManager creates a Sender for a kiss.Manager using its NodeName.
func NewForManager(m *kiss.Manager) *Sender {
	return NewSender(m, m.NodeName)
}
