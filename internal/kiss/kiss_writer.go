package kiss

import (
	"log/slog"
	"time"
)

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
	var inFlight []byte
	var retries int
	var gateTimer *time.Timer
	var gateC <-chan time.Time
	var busyTimer *time.Timer
	var busyC <-chan time.Time
	stopTimer := func(timer **time.Timer, channel *<-chan time.Time) {
		if *timer != nil {
			if !(*timer).Stop() {
				select {
				case <-(*timer).C:
				default:
				}
			}
			*timer = nil
			*channel = nil
		}
	}
	release := func() {
		inFlight = nil
		retries = 0
		stopTimer(&gateTimer, &gateC)
		stopTimer(&busyTimer, &busyC)
	}
	startGateTimer := func() {
		stopTimer(&gateTimer, &gateC)
		gateTimer = time.NewTimer(m.txGateTimeout())
		gateC = gateTimer.C
	}
	startBusyTimer := func() {
		stopTimer(&busyTimer, &busyC)
		busyTimer = time.NewTimer(500 * time.Millisecond)
		busyC = busyTimer.C
	}
	defer release()

	for {
		select {
		case <-m.stopChan:
			return
		case control := <-m.controlChan:
			m.writeControl(control)
			continue
		default:
		}

		select {
		case <-m.stopChan:
			return
		case control := <-m.controlChan:
			m.writeControl(control)
		default:
		}

		var dataFrames <-chan []byte
		if inFlight == nil {
			dataFrames = m.txChan
		}
		select {
		case <-m.stopChan:
			return
		case control := <-m.controlChan:
			m.writeControl(control)
		case frame := <-dataFrames:
			payloadLength := len(frame) - 1
			if len(frame) == 0 || frame[0] != CMD_DATA || payloadLength < 1 || payloadLength > KISSMaxDataSize {
				m.counters.txRejectedLength.Add(1)
				slog.Warn("rejecting invalid KISS data frame", "length", payloadLength)
				continue
			}
			copyFrame := append([]byte(nil), frame...)
			if err := m.writeFrame(copyFrame); err != nil {
				continue
			}
			inFlight = copyFrame
			retries = 0
			startGateTimer()
		case event := <-m.txGateEvents:
			if inFlight == nil {
				continue
			}
			if event.done {
				release()
			} else if event.busy {
				if retries == 0 {
					retries = 1
					startBusyTimer()
				} else {
					slog.Warn("dropping KISS data frame after repeated TxBusy")
					release()
					m.publishModemEvent(TxAbandoned{})
				}
			}
		case <-busyC:
			busyTimer = nil
			busyC = nil
			if inFlight != nil && retries == 1 {
				if err := m.writeFrame(inFlight); err != nil {
					release()
					continue
				}
				m.counters.txBusyRetry.Add(1)
				startGateTimer()
			}
		case <-gateC:
			m.counters.txGateTimeout.Add(1)
			slog.Warn("KISS TX gate timed out; releasing pending data frame")
			release()
			m.publishModemEvent(TxAbandoned{})
		}
	}
}

func (m *Manager) writeControl(control controlFrame) {
	err := m.writeFrame(control.frame)
	if control.done != nil {
		control.done <- err
	}
}

func (m *Manager) writeFrame(frame []byte) error {
	if m.writeHook != nil {
		return m.writeHook(frame)
	}
	framedData := Frame(frame)

	// Write the framed data to the serial port
	_, err := m.stream.Write(framedData)
	if err != nil {
		slog.Error("Error writing to serial port", "err", err)
		select {
		case <-m.stopChan:
		default:
			m.Fail(err)
		}
	} else {
		slog.Info("Frame sent", "frame", framedData)
	}
	return err
}
