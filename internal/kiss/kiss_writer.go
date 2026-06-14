package kiss

import "log/slog"

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

	// Frame the data according to KISS protocol
	framedData := []byte{FEND}
	for _, b := range frame {
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

	// Write the framed data to the serial port
	_, err := m.stream.Write(framedData)
	if err != nil {
		slog.Error("Error writing to serial port", "err", err)
	} else {
		slog.Info("Frame sent", "frame", framedData)
	}
	return err
}
