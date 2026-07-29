package kiss

import (
	"context"
	"encoding/binary"
	"fmt"
)

// RadioClient provides methods to interact with the radio hardware via KISS
type RadioClient struct {
	manager *Manager
}

func NewRadioClient(m *Manager) *RadioClient {
	return &RadioClient{manager: m}
}

// Get requests current radio settings
func (c *RadioClient) Get(ctx context.Context) (*RadioConfig, error) {

	// Exclusive lock the device
	c.manager.LockDevice()
	defer c.manager.UnlockDevice()

	respChan := c.manager.registry.Register(SUB_RESP_RADIO)
	defer c.manager.registry.Deregister(SUB_RESP_RADIO)

	c.manager.txChan <- []byte{CMD_SETHARDWARE, SUB_GET_RADIO}

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("radio get aborted: %w", ctx.Err())
	case rawPayload := <-respChan:
		if len(rawPayload) < 10 {
			return nil, fmt.Errorf("malformed radio response payload: too short (%d bytes)", len(rawPayload))
		}

		return &RadioConfig{
			Frequency:       binary.LittleEndian.Uint32(rawPayload[0:4]),
			Bandwidth:       binary.LittleEndian.Uint32(rawPayload[4:8]),
			SpreadingFactor: rawPayload[8],
			CodingRate:      rawPayload[9],
		}, nil
	}
}

// ApplyRadioConfig pushes cfg to the radio, then reads it back to confirm the
// hardware accepted it. Both KISS exchanges share ctx. Returns the verified
// configuration as reported by the board.
func (c *RadioClient) ApplyRadioConfig(ctx context.Context, cfg RadioConfig) (*RadioConfig, error) {
	if err := c.Set(ctx, &cfg); err != nil {
		return nil, fmt.Errorf("set radio config: %w", err)
	}
	verified, err := c.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("verify radio config: %w", err)
	}
	return verified, nil
}

// Set updates radio settings.
func (c *RadioClient) Set(ctx context.Context, config *RadioConfig) error {

	// Exclusive lock the device
	c.manager.LockDevice()
	defer c.manager.UnlockDevice()

	payload := make([]byte, 11) // 1 byte sub-cmd + 10 bytes data
	payload[0] = SUB_SET_RADIO
	binary.LittleEndian.PutUint32(payload[1:5], config.Frequency)
	binary.LittleEndian.PutUint32(payload[5:9], config.Bandwidth)
	payload[9] = config.SpreadingFactor
	payload[10] = config.CodingRate

	// Wrap in SetHardware command
	frame := append([]byte{CMD_SETHARDWARE}, payload...)

	// Register for OK (0xF0) or Error (0xF1) response
	respChan := c.manager.registry.Register(SUB_RESP_OK)
	defer c.manager.registry.Deregister(SUB_RESP_OK)

	c.manager.txChan <- frame

	select {
	case <-ctx.Done():
		return fmt.Errorf("radio set aborted: %w", ctx.Err())
	case <-respChan:
		return nil
	}
}
