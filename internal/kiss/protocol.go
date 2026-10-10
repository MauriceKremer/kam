package kiss

import (
	"encoding/binary"
	"fmt"
)

const (
	FEND  = 0xC0
	FESC  = 0xDB
	TFEND = 0xDC
	TFESC = 0xDD
)

const (
	CMD_DATA        = 0x00
	CMD_TXDELAY     = 0x01
	CMD_PERSISTENCE = 0x02
	CMD_SLOTTIME    = 0x03
	CMD_TXTAIL      = 0x04
	CMD_FULLDUPLEX  = 0x05
	CMD_SETHARDWARE = 0x06
	CMD_RETURN      = 0xFF
)

const (
	SUB_GET_IDENTITY      = 0x01
	SUB_GET_RANDOM        = 0x02
	SUB_VERIFY_SIGNATURE  = 0x03
	SUB_SIGN_DATA         = 0x04
	SUB_ENCRYPT_DATA      = 0x05
	SUB_DECRYPT_DATA      = 0x06
	SUB_KEY_EXCHANGE      = 0x07
	SUB_HASH              = 0x08
	SUB_SET_RADIO         = 0x09
	SUB_SET_TX_POWER      = 0x0A
	SUB_GET_RADIO         = 0x0B
	SUB_GET_TX_POWER      = 0x0C
	SUB_GET_CURRENT_RSSI  = 0x0D
	SUB_IS_CHANNEL_BUSY   = 0x0E
	SUB_GET_AIRTIME       = 0x0F
	SUB_GET_NOISE_FLOOR   = 0x10
	SUB_GET_VERSION       = 0x11
	SUB_GET_STATS         = 0x12
	SUB_GET_BATTERY       = 0x13
	SUB_GET_MCU_TEMP      = 0x14
	SUB_GET_SENSORS       = 0x15
	SUB_GET_DEVICE_NAME   = 0x16
	SUB_PING              = 0x17
	SUB_REBOOT            = 0x18
	SUB_SET_SIGNAL_REPORT = 0x19
	SUB_GET_SIGNAL_REPORT = 0x1A
)

const (
	SUB_RESP_RADIO        = 0x8B
	SUB_RESP_TX_POWER     = 0x8C
	SUB_RESP_CURRENT_RSSI = 0x8D
	SUB_RESP_CHANNEL_BUSY = 0x8E
	SUB_RESP_AIRTIME      = 0x8F
	SUB_RESP_NOISE_FLOOR  = 0x90
	SUB_RESP_VERSION      = 0x91
	SUB_RESP_STATS        = 0x92
	SUB_RESP_BATTERY      = 0x93
	SUB_RESP_MCU_TEMP     = 0x94
	SUB_RESP_SENSORS      = 0x95
	SUB_RESP_DEVICE_NAME  = 0x96
	SUB_RESP_PONG         = 0x97
	SUB_RESP_SIGNAL       = 0x9A
	SUB_RESP_OK           = 0xF0
	SUB_ERROR             = 0xF1
	SUB_TX_DONE           = 0xF8
	SUB_RX_META           = 0xF9
)

const (
	MODEM_ERROR_INVALID_LENGTH = 0x01
	MODEM_ERROR_INVALID_PARAM  = 0x02
	MODEM_ERROR_NO_CALLBACK    = 0x03
	MODEM_ERROR_MAC_FAILED     = 0x04
	MODEM_ERROR_UNKNOWN_CMD    = 0x05
	MODEM_ERROR_ENCRYPT_FAILED = 0x06
	MODEM_ERROR_TX_BUSY        = 0x07
)

const (
	KISSMaxFrameSize = 512
	KISSMaxDataSize  = 255
)

type FirmwareVersion struct {
	Version  byte
	Reserved byte
}

type ModemStats struct {
	RX     uint32
	TX     uint32
	Errors uint32
}

func encodeHardwareRequest(command byte, data ...byte) []byte {
	frame := make([]byte, 2, 2+len(data))
	frame[0], frame[1] = CMD_SETHARDWARE, command
	return append(frame, data...)
}

func encodeSetRadioRequest(config RadioConfig) []byte {
	frame := make([]byte, 12)
	frame[0], frame[1] = CMD_SETHARDWARE, SUB_SET_RADIO
	binary.LittleEndian.PutUint32(frame[2:6], config.Frequency)
	binary.LittleEndian.PutUint32(frame[6:10], config.Bandwidth)
	frame[10], frame[11] = config.SpreadingFactor, config.CodingRate
	return frame
}

func decodeRadioResponse(payload []byte) (RadioConfig, error) {
	if len(payload) < 10 {
		return RadioConfig{}, fmt.Errorf("%w: radio payload has %d bytes, want at least 10", ErrMalformedResponse, len(payload))
	}
	return RadioConfig{
		Frequency:       binary.LittleEndian.Uint32(payload[0:4]),
		Bandwidth:       binary.LittleEndian.Uint32(payload[4:8]),
		SpreadingFactor: payload[8],
		CodingRate:      payload[9],
	}, nil
}

func decodeUint16Response(payload []byte, name string) (uint16, error) {
	if len(payload) < 2 {
		return 0, fmt.Errorf("%w: %s payload has %d bytes, want at least 2", ErrMalformedResponse, name, len(payload))
	}
	return binary.LittleEndian.Uint16(payload[:2]), nil
}

func decodeInt16Response(payload []byte, name string) (int16, error) {
	value, err := decodeUint16Response(payload, name)
	return int16(value), err
}

func decodeByteResponse(payload []byte, name string) (byte, error) {
	if len(payload) < 1 {
		return 0, fmt.Errorf("%w: %s payload is empty", ErrMalformedResponse, name)
	}
	return payload[0], nil
}

func decodeInt8Response(payload []byte, name string) (int8, error) {
	value, err := decodeByteResponse(payload, name)
	return int8(value), err
}

func decodeBoolResponse(payload []byte, name string) (bool, error) {
	value, err := decodeByteResponse(payload, name)
	if err != nil {
		return false, err
	}
	if value > 1 {
		return false, fmt.Errorf("%w: %s boolean value is %d", ErrMalformedResponse, name, value)
	}
	return value == 1, nil
}

func decodeUint32Response(payload []byte, name string) (uint32, error) {
	if len(payload) < 4 {
		return 0, fmt.Errorf("%w: %s payload has %d bytes, want at least 4", ErrMalformedResponse, name, len(payload))
	}
	return binary.LittleEndian.Uint32(payload[:4]), nil
}

func decodeVersionResponse(payload []byte) (FirmwareVersion, error) {
	if len(payload) < 2 {
		return FirmwareVersion{}, fmt.Errorf("%w: version payload has %d bytes, want at least 2", ErrMalformedResponse, len(payload))
	}
	return FirmwareVersion{Version: payload[0], Reserved: payload[1]}, nil
}

func decodeStatsResponse(payload []byte) (ModemStats, error) {
	if len(payload) < 12 {
		return ModemStats{}, fmt.Errorf("%w: stats payload has %d bytes, want at least 12", ErrMalformedResponse, len(payload))
	}
	return ModemStats{
		RX: binary.LittleEndian.Uint32(payload[0:4]), TX: binary.LittleEndian.Uint32(payload[4:8]),
		Errors: binary.LittleEndian.Uint32(payload[8:12]),
	}, nil
}

func decodeSensorsResponse(payload []byte) []byte { return append([]byte(nil), payload...) }

func decodeDeviceNameResponse(payload []byte) string { return string(payload) }
