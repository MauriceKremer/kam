package kiss

import "log/slog"

func (m *Manager) readLoop() {

	var frameBuffer []byte
	inEscape := false
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

		for i := 0; i < n; i++ {
			b := readBuffer[i]
			switch b {
			case FEND:
				if len(frameBuffer) > 0 {
					// Create a copy of the frameBuffer to send to the rxChan
					frameCopy := make([]byte, len(frameBuffer))
					copy(frameCopy, frameBuffer)
					m.rxChan <- frameCopy
					frameBuffer = frameBuffer[:0] // Reset the buffer
					slog.Debug("KissReader - Frame received", "frame", frameCopy)
				}
			case FESC:
				inEscape = true
			default:
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
