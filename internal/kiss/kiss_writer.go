package kiss

import "log/slog"

func (m *Manager) writeLoop() {

	for frame := range m.txChan {
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
	}

}
