package kiss

import (
	"context"
)

func (m *Manager) parseAndStoreLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case frame := <-m.rxChan:
			if len(frame) == 0 {
				continue
			}

			cmdByte := frame[0]
			payload := frame[1:]

			switch cmdByte {
			case CMD_SETHARDWARE:
				if len(payload) == 0 {
					continue
				}
				subCmd := payload[0]
				subPayload := payload[1:]
				// Dispatch by sub-command for SetHardware extensions
				m.registry.Dispatch(subCmd, subPayload)

			case CMD_DATA:
				// Handle standard incoming mesh text/data packets...
			}
		}
	}
}
