package kiss

import "log/slog"

// Frame wraps a raw KISS payload with FEND framing and byte stuffing.
func Frame(payload []byte) []byte {
	framedData := []byte{FEND}
	for _, b := range payload {
		switch b {
		case FEND:
			framedData = append(framedData, FESC, TFEND)
		case FESC:
			framedData = append(framedData, FESC, TFESC)
		default:
			framedData = append(framedData, b)
		}
	}
	framedData = append(framedData, FEND)
	return framedData
}

func (m *Manager) writeLoop() {
	for {
		select {
		case <-m.stopChan:
			return
		case frame := <-m.txChan:
			m.writeFrame(frame)
		}
	}
}

func (m *Manager) writeFrame(frame []byte) error {
	framedData := Frame(frame)

	// Write the framed data to the serial port
	_, err := m.stream.Write(framedData)
	if err != nil {
		slog.Error("Error writing to serial port", "err", err)
	} else {
		slog.Info("Frame sent", "frame", framedData)
	}
	return err
}
