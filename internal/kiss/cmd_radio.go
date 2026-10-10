package kiss

import (
	"context"
	"errors"
	"fmt"
	"time"

	"kam.bytecoder.nl/internal/models"
)

var (
	ErrInvalidLength     = errors.New("modem rejected invalid length")
	ErrInvalidParam      = errors.New("modem rejected invalid parameter")
	ErrNoCallback        = errors.New("modem command has no callback")
	ErrUnknownCmd        = errors.New("modem does not recognize command")
	ErrMacFailed         = errors.New("modem MAC verification failed")
	ErrEncryptFailed     = errors.New("modem encryption failed")
	ErrTxBusy            = errors.New("modem transmitter busy")
	ErrUnsupported       = errors.New("modem command unsupported")
	ErrMalformedResponse = errors.New("malformed modem response")
	ErrModemUnresponsive = errors.New("modem did not respond to ping barrier")
)

const defaultCommandTimeout = time.Second

// ModemError reports a KISS modem Error response.
type ModemError struct{ Code byte }

func (e *ModemError) Error() string { return fmt.Sprintf("KISS modem error 0x%02X", e.Code) }

func (e *ModemError) Is(target error) bool {
	switch e.Code {
	case MODEM_ERROR_INVALID_LENGTH:
		return target == ErrInvalidLength
	case MODEM_ERROR_INVALID_PARAM:
		return target == ErrInvalidParam
	case MODEM_ERROR_NO_CALLBACK:
		return target == ErrNoCallback || target == ErrUnsupported
	case MODEM_ERROR_MAC_FAILED:
		return target == ErrMacFailed
	case MODEM_ERROR_UNKNOWN_CMD:
		return target == ErrUnknownCmd || target == ErrUnsupported
	case MODEM_ERROR_ENCRYPT_FAILED:
		return target == ErrEncryptFailed
	case MODEM_ERROR_TX_BUSY:
		return target == ErrTxBusy
	default:
		return false
	}
}

// RadioClient provides methods to interact with the radio hardware via KISS
type RadioClient struct {
	manager *Manager
}

func NewRadioClient(m *Manager) *RadioClient {
	return &RadioClient{manager: m}
}

func (c *RadioClient) exchange(ctx context.Context, frame []byte, expected ...byte) ([]byte, error) {
	ctx, cancel := commandContext(ctx)
	defer cancel()

	select {
	case c.manager.requestGate <- struct{}{}:
		defer func() { <-c.manager.requestGate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	if c.manager.isUnsynced() {
		if err := c.pingBarrier(ctx); err != nil {
			return nil, fmt.Errorf("resynchronize modem: %w", err)
		}
	}
	return c.exchangeLocked(ctx, frame, expected, false)
}

func commandContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, defaultCommandTimeout)
}

func (c *RadioClient) exchangeLocked(ctx context.Context, frame []byte, expected []byte, barrier bool) ([]byte, error) {
	waiter := &pendingRequest{expected: make(map[byte]struct{}, len(expected)), result: make(chan requestResult, 1)}
	for _, command := range expected {
		waiter.expected[command] = struct{}{}
	}

	c.manager.requestMu.Lock()
	c.manager.pending = waiter
	if barrier {
		c.manager.barrier = true
	}
	c.manager.requestMu.Unlock()

	if err := c.manager.sendControl(ctx, frame); err != nil {
		c.manager.clearRequest(waiter, true)
		return nil, err
	}

	select {
	case result := <-waiter.result:
		c.manager.clearRequest(waiter, false)
		return result.payload, result.err
	case <-ctx.Done():
		c.manager.clearRequest(waiter, true)
		return nil, ctx.Err()
	}
}

func (m *Manager) sendControl(ctx context.Context, frame []byte) error {
	done := make(chan error, 1)
	control := controlFrame{frame: append([]byte(nil), frame...), done: done}
	select {
	case m.controlChan <- control:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *RadioClient) pingBarrier(ctx context.Context) error {
	_, err := c.exchangeLocked(ctx, []byte{CMD_SETHARDWARE, SUB_PING}, []byte{SUB_RESP_PONG}, true)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("%w: %v", ErrModemUnresponsive, err)
		}
		return err
	}
	return nil
}

func (m *Manager) isUnsynced() bool {
	m.requestMu.Lock()
	defer m.requestMu.Unlock()
	return m.unsynced
}

func (m *Manager) clearRequest(request *pendingRequest, unsynced bool) {
	m.requestMu.Lock()
	defer m.requestMu.Unlock()
	if m.pending == request {
		m.pending = nil
	}
	if unsynced {
		m.unsynced = true
	}
}

// Get requests the modem's cached host-set radio settings.
func (c *RadioClient) Get(ctx context.Context) (*RadioConfig, error) {
	config, err := c.GetRadio(ctx)
	if err != nil {
		return nil, err
	}
	return &config, nil
}

// GetRadio returns the modem's cached host-set radio values, not hardware readback.
func (c *RadioClient) GetRadio(ctx context.Context) (RadioConfig, error) {
	rawPayload, err := c.exchange(ctx, []byte{CMD_SETHARDWARE, SUB_GET_RADIO}, SUB_RESP_RADIO)
	if err != nil {
		return RadioConfig{}, err
	}
	return decodeRadioResponse(rawPayload)
}

// ApplyRadioConfig pushes cfg to the modem and compares the modem's cached echo.
// That echo confirms receipt, not that the RF chip accepted the settings.
func (c *RadioClient) ApplyRadioConfig(ctx context.Context, cfg RadioConfig) (*RadioConfig, error) {
	if err := c.SetRadio(ctx, cfg); err != nil {
		return nil, fmt.Errorf("set radio config: %w", err)
	}
	verified, err := c.GetRadio(ctx)
	if err != nil {
		return nil, fmt.Errorf("verify radio config: %w", err)
	}
	return &verified, nil
}

// Set updates radio settings.
func (c *RadioClient) Set(ctx context.Context, config *RadioConfig) error {
	if config == nil {
		return fmt.Errorf("radio config is nil")
	}
	return c.SetRadio(ctx, *config)
}

func (c *RadioClient) SetRadio(ctx context.Context, config RadioConfig) error {
	if err := models.ValidateRadioParameters(config.Frequency, config.Bandwidth, config.SpreadingFactor, config.CodingRate); err != nil {
		return err
	}
	_, err := c.exchange(ctx, encodeSetRadioRequest(config), SUB_RESP_OK)
	return err
}

func (c *RadioClient) SetTxPower(ctx context.Context, dbm uint8) error {
	if err := models.ValidateTxPowerDBm(dbm); err != nil {
		return err
	}
	_, err := c.exchange(ctx, encodeHardwareRequest(SUB_SET_TX_POWER, dbm), SUB_RESP_OK)
	return err
}

// GetTxPower returns the modem's cached host-set power, not a hardware reading.
func (c *RadioClient) GetTxPower(ctx context.Context) (uint8, error) {
	payload, err := c.exchange(ctx, encodeHardwareRequest(SUB_GET_TX_POWER), SUB_RESP_TX_POWER)
	if err != nil {
		return 0, err
	}
	return decodeByteResponse(payload, "TX power")
}

func (c *RadioClient) Ping(ctx context.Context) error {
	_, err := c.exchange(ctx, encodeHardwareRequest(SUB_PING), SUB_RESP_PONG)
	return err
}

func (c *RadioClient) SetSignalReport(ctx context.Context, enabled bool) (bool, error) {
	value := byte(0)
	if enabled {
		value = 1
	}
	payload, err := c.exchange(ctx, encodeHardwareRequest(SUB_SET_SIGNAL_REPORT, value), SUB_RESP_SIGNAL)
	if err != nil {
		return false, err
	}
	return decodeBoolResponse(payload, "signal report")
}

func (c *RadioClient) GetSignalReport(ctx context.Context) (bool, error) {
	payload, err := c.exchange(ctx, encodeHardwareRequest(SUB_GET_SIGNAL_REPORT), SUB_RESP_SIGNAL)
	if err != nil {
		return false, err
	}
	return decodeBoolResponse(payload, "signal report")
}

func (c *RadioClient) SetTxDelay(ctx context.Context, milliseconds int) error {
	if milliseconds < 0 || milliseconds > 2550 || milliseconds%10 != 0 {
		return fmt.Errorf("TXDELAY must be a multiple of 10 between 0 and 2550 ms, got %d", milliseconds)
	}
	return c.setKISSParameter(ctx, CMD_TXDELAY, byte(milliseconds/10))
}

func (c *RadioClient) SetPersistence(ctx context.Context, value uint8) error {
	return c.setKISSParameter(ctx, CMD_PERSISTENCE, value)
}

func (c *RadioClient) SetSlotTime(ctx context.Context, milliseconds int) error {
	if milliseconds < 0 || milliseconds > 2550 || milliseconds%10 != 0 {
		return fmt.Errorf("slot time must be a multiple of 10 between 0 and 2550 ms, got %d", milliseconds)
	}
	return c.setKISSParameter(ctx, CMD_SLOTTIME, byte(milliseconds/10))
}

func (c *RadioClient) SetTxTail(ctx context.Context, milliseconds int) error {
	if milliseconds < 0 || milliseconds > 2550 || milliseconds%10 != 0 {
		return fmt.Errorf("TXtail must be a multiple of 10 between 0 and 2550 ms, got %d", milliseconds)
	}
	return c.setKISSParameter(ctx, CMD_TXTAIL, byte(milliseconds/10))
}

func (c *RadioClient) SetFullDuplex(ctx context.Context, enabled bool) error {
	value := byte(0)
	if enabled {
		value = 1
	}
	return c.setKISSParameter(ctx, CMD_FULLDUPLEX, value)
}

func (c *RadioClient) setKISSParameter(ctx context.Context, command, value byte) error {
	ctx, cancel := commandContext(ctx)
	defer cancel()
	select {
	case c.manager.requestGate <- struct{}{}:
		defer func() { <-c.manager.requestGate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if c.manager.isUnsynced() {
		if err := c.pingBarrier(ctx); err != nil {
			return err
		}
	}
	if err := c.manager.sendControl(ctx, []byte{command, value}); err != nil {
		c.manager.markUnsynced()
		return err
	}
	config := c.manager.csmaConfig()
	switch command {
	case CMD_TXDELAY:
		config.TxDelayMS = int(value) * 10
	case CMD_PERSISTENCE:
		config.Persistence = value
	case CMD_SLOTTIME:
		config.SlotTimeMS = int(value) * 10
	case CMD_FULLDUPLEX:
		config.FullDuplex = value != 0
	}
	c.manager.setCSMA(config)
	return nil
}

func (c *RadioClient) SetCSMA(ctx context.Context, config CSMAConfig) error {
	if err := config.Validate(); err != nil {
		return err
	}
	ctx, cancel := commandContext(ctx)
	defer cancel()
	select {
	case c.manager.requestGate <- struct{}{}:
		defer func() { <-c.manager.requestGate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if c.manager.isUnsynced() {
		if err := c.pingBarrier(ctx); err != nil {
			return err
		}
	}
	values := []struct{ command, value byte }{
		{CMD_TXDELAY, byte(config.TxDelayMS / 10)},
		{CMD_PERSISTENCE, byte(config.Persistence)},
		{CMD_SLOTTIME, byte(config.SlotTimeMS / 10)},
	}
	fullDuplex := byte(0)
	if config.FullDuplex {
		fullDuplex = 1
	}
	values = append(values, struct{ command, value byte }{CMD_FULLDUPLEX, fullDuplex})
	for _, value := range values {
		if err := c.manager.sendControl(ctx, []byte{value.command, value.value}); err != nil {
			c.manager.markUnsynced()
			return err
		}
	}
	c.manager.setCSMA(config)
	return c.pingBarrier(ctx)
}

type CSMAConfig struct {
	TxDelayMS   int
	Persistence uint8
	SlotTimeMS  int
	FullDuplex  bool
}

func (config CSMAConfig) Validate() error {
	return (models.CSMAConfig{
		TxDelayMS: config.TxDelayMS, Persistence: int(config.Persistence), SlotTimeMS: config.SlotTimeMS,
		FullDuplex: config.FullDuplex,
	}).Validate()
}
