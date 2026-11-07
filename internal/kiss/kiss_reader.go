package kiss

import "log/slog"

func (m *Manager) readLoop() {

	var frameBuffer []byte
	inEscape := false
	discarding := false // overflow: drop bytes until the next FEND
	readBuffer := make([]byte, 256)

	for {
		n, err := m.stream.Read(readBuffer)
		if err != nil {
			select {
			case <-m.stopChan:
				return // graceful shutdown via Stop()
			default:
				slog.Error("Error reading from serial port", "err", err)
				m.Fail(err)
				return
			}
		}

		slog.Debug("Read bytes from serial port", "n", n, "data", readBuffer[:n])

		for i := range n {
			b := readBuffer[i]
			switch b {
			case FEND:
				if len(frameBuffer) > 0 {
					// Create a copy of the frameBuffer to send to the rxChan
					frameCopy := make([]byte, len(frameBuffer))
					copy(frameCopy, frameBuffer)
					m.rxChan <- frameCopy
					slog.Debug("KissReader - Frame received", "frame", frameCopy)
				}
				frameBuffer = frameBuffer[:0] // Reset the buffer
				discarding = false
			case FESC:
				if !discarding {
					inEscape = true
				}
			default:
				if !discarding && len(frameBuffer) >= KISSMaxFrameSize {
					discarding = true
					inEscape = false
					frameBuffer = frameBuffer[:0]
					slog.Warn("[KissReader] oversized frame discarded; waiting for FEND",
						"limit", KISSMaxFrameSize)
				}
				if discarding {
					break
				}
				if inEscape {
					switch b {
					case TFEND:
						frameBuffer = append(frameBuffer, FEND)
					case TFESC:
						frameBuffer = append(frameBuffer, FESC)
					default:
						frameBuffer = append(frameBuffer, b)
					}
					inEscape = false
				} else {
					frameBuffer = append(frameBuffer, b)
				}
			}
		}
	}

}
