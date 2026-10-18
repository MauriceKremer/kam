package kiss

import (
	"context"
	"fmt"
)

func (c *RadioClient) GetCurrentRSSI(ctx context.Context) (int8, error) {
	payload, err := c.exchange(ctx, encodeHardwareRequest(SUB_GET_CURRENT_RSSI), SUB_RESP_CURRENT_RSSI)
	if err != nil {
		return 0, err
	}
	return decodeInt8Response(payload, "RSSI")
}

func (c *RadioClient) IsChannelBusy(ctx context.Context) (bool, error) {
	payload, err := c.exchange(ctx, encodeHardwareRequest(SUB_IS_CHANNEL_BUSY), SUB_RESP_CHANNEL_BUSY)
	if err != nil {
		return false, err
	}
	return decodeBoolResponse(payload, "channel busy")
}

func (c *RadioClient) AirtimeMillis(ctx context.Context, length uint8) (uint32, error) {
	if length == 0 {
		return 0, fmt.Errorf("packet length must be between 1 and 255 bytes")
	}
	payload, err := c.exchange(ctx, encodeHardwareRequest(SUB_GET_AIRTIME, length), SUB_RESP_AIRTIME)
	if err != nil {
		return 0, err
	}
	return decodeUint32Response(payload, "airtime")
}

func (c *RadioClient) NoiseFloor(ctx context.Context) (int16, bool, error) {
	payload, err := c.exchange(ctx, encodeHardwareRequest(SUB_GET_NOISE_FLOOR), SUB_RESP_NOISE_FLOOR)
	if err != nil {
		return 0, false, err
	}
	dBm, err := decodeInt16Response(payload, "noise-floor")
	if err != nil {
		return 0, false, err
	}
	return dBm, dBm != 0, nil
}

func (c *RadioClient) GetVersion(ctx context.Context) (FirmwareVersion, error) {
	payload, err := c.exchange(ctx, encodeHardwareRequest(SUB_GET_VERSION), SUB_RESP_VERSION)
	if err != nil {
		return FirmwareVersion{}, err
	}
	version, err := decodeVersionResponse(payload)
	if err == nil {
		c.identityMu.Lock()
		c.version = &version.Version
		c.identityMu.Unlock()
	}
	return version, err
}

func (c *RadioClient) GetStats(ctx context.Context) (ModemStats, error) {
	payload, err := c.exchange(ctx, encodeHardwareRequest(SUB_GET_STATS), SUB_RESP_STATS)
	if err != nil {
		return ModemStats{}, err
	}
	return decodeStatsResponse(payload)
}

func (c *RadioClient) BatteryMillivolts(ctx context.Context) (uint16, error) {
	payload, err := c.exchange(ctx, encodeHardwareRequest(SUB_GET_BATTERY), SUB_RESP_BATTERY)
	if err != nil {
		return 0, err
	}
	return decodeUint16Response(payload, "battery")
}

func (c *RadioClient) MCUTempTenthsCelsius(ctx context.Context) (int16, error) {
	payload, err := c.exchange(ctx, encodeHardwareRequest(SUB_GET_MCU_TEMP), SUB_RESP_MCU_TEMP)
	if err != nil {
		return 0, err
	}
	return decodeInt16Response(payload, "MCU temperature")
}

func (c *RadioClient) Sensors(ctx context.Context, permissions uint8) ([]byte, error) {
	if permissions&^uint8(0x07) != 0 {
		return nil, fmt.Errorf("sensor permissions contain unsupported bits: 0x%02X", permissions)
	}
	payload, err := c.exchange(ctx, encodeHardwareRequest(SUB_GET_SENSORS, permissions), SUB_RESP_SENSORS)
	if err != nil {
		return nil, err
	}
	return decodeSensorsResponse(payload), nil
}

func (c *RadioClient) DeviceName(ctx context.Context) (string, error) {
	payload, err := c.exchange(ctx, encodeHardwareRequest(SUB_GET_DEVICE_NAME), SUB_RESP_DEVICE_NAME)
	if err != nil {
		return "", err
	}
	name := decodeDeviceNameResponse(payload)
	c.identityMu.Lock()
	c.deviceName = &name
	c.identityMu.Unlock()
	return name, nil
}
