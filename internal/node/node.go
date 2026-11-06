// Package node owns the lifecycle of a running KAM node: a KISS TNC manager
// wired to SQLite persistence, a transmit scheduler for every outbound packet,
// the group-text sender, optional room servers, and a verified radio
// configuration. Construction (New) and the blocking run loop (Run) are the
// only entry points; flag parsing, logging setup, and signal handling stay in
// the caller.
package node

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"kam.bytecoder.nl/internal/automation"
	"kam.bytecoder.nl/internal/config"
	"kam.bytecoder.nl/internal/db"
	"kam.bytecoder.nl/internal/kiss"
	"kam.bytecoder.nl/internal/kiss/send"
	"kam.bytecoder.nl/internal/models"
	"kam.bytecoder.nl/internal/room"
)

const (
	radioStartupTimeout = 10 * time.Second
	radioProbeInterval  = 30 * time.Second
)

// Node is a running KAM node. A Node is constructed by New and run by Run;
// zero values are not valid.
type Node struct {
	manager              *kiss.Manager
	radio                *kiss.RadioClient
	scheduler            *kiss.TxScheduler
	sender               *send.Sender
	automation           *automation.Runtime
	automationRepository *db.AutomationRepository
	rooms                *room.Service
	roomRepository       *db.RoomRepository
	unsubscribePackets   func()
	monitorServer        *http.Server
	unsubscribe          func()
	configPath           string
	modem                *modemController
	radioState           *radioRuntime
}

// radioRuntime is the node's radio truth: the configured settings (as saved
// in kam.yml, port and baud included) and the modem's last confirmed echo. A
// radio change publishes both; the reset probe, the monitor and the
// scheduler's airtime estimate read from it.
type radioRuntime struct {
	mu       sync.Mutex
	settings models.RadioConfig
	echo     *kiss.RadioConfig
}

func (rt *radioRuntime) settingsValue() models.RadioConfig {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.settings
}

// echoValue returns a copy of the last confirmed echo, or nil.
func (rt *radioRuntime) echoValue() *kiss.RadioConfig {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.echo == nil {
		return nil
	}
	value := *rt.echo
	return &value
}

// publish records a confirmed echo and the settings that belong to it; a nil
// echo keeps the previous one.
func (rt *radioRuntime) publish(echo *kiss.RadioConfig, settings models.RadioConfig) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if echo != nil {
		value := *echo
		rt.echo = &value
	}
	rt.settings = settings
}

// New bootstraps the channel registry from kam.yml, opens persistence,
// opens and starts the KISS manager, and pushes + verifies the radio
// configuration. configPath is the YAML file used as the single source of
// truth for system configuration; runtime channel additions are written back
// to it. On success the returned Node is ready to Run. On failure any opened
// resources are torn down and a wrapped error is returned.
func New(cfg *models.KamConfig, dbPath, configPath string) (*Node, error) {
	if err := config.BootstrapChannels(cfg); err != nil {
		return nil, fmt.Errorf("bootstrap channels: %w", err)
	}

	persistence, err := db.NewPersistence(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open persistence: %w", err)
	}

	manager, err := kiss.NewManager(cfg.Radio.Port, cfg.Radio.Baud)
	if err != nil {
		// persistence is the only resource opened so far; manager.Stop would
		// also close it, but the manager itself failed to construct, so close
		// persistence directly.
		_ = persistence.Close()
		return nil, fmt.Errorf("open serial port %q: %w", cfg.Radio.Port, err)
	}
	manager.Store = persistence
	manager.NodeName = cfg.NodeName
	manager.Start()

	radio := kiss.NewRadioClient(manager)

	ctx, cancel := context.WithTimeout(context.Background(), radioStartupTimeout)
	defer cancel()
	probes, err := configureRadio(ctx, radio, cfg)
	if err != nil {
		// manager.Stop flushes pending writes and closes the persistence store
		// and serial port, so no separate cleanup is needed.
		manager.Stop()
		return nil, fmt.Errorf("apply radio config: %w", err)
	}
	radioState := &radioRuntime{settings: cfg.Radio, echo: probes.verified}

	meshCfg, roomConfigs, problems := cfg.ResolveRooms()
	for _, problem := range problems {
		slog.Error("[Node] room configuration problem", "error", problem)
	}

	// One scheduler owns the radio data path for every producer: group texts
	// from the sender and automation, and everything the rooms transmit.
	scheduler := kiss.NewTxScheduler(manager, kiss.TxSchedulerConfig{
		DutyCyclePercent: meshCfg.DutyCyclePercent,
		Airtime: func(packetBytes int) time.Duration {
			if echo := radioState.echoValue(); echo != nil {
				return kiss.LoRaAirtime(echo.SpreadingFactor, echo.Bandwidth, echo.CodingRate, packetBytes)
			}
			return 0
		},
	})
	scheduler.Start()
	sender := send.NewSender(scheduler, manager.NodeName)

	// A room that cannot start never stops radio ingestion.
	var roomService *room.Service
	var roomRepository *db.RoomRepository
	var unsubscribePackets func()
	if len(roomConfigs) > 0 {
		service, repository, err := openRooms(dbPath, meshCfg, roomConfigs, scheduler)
		if err != nil {
			slog.Error("[Node] rooms disabled", "error", err)
		} else {
			roomService, roomRepository = service, repository
			service.Start()
			unsubscribePackets = manager.SubscribePackets(service.HandlePacket)
		}
	}

	var automationRuntime *automation.Runtime
	var automationRepository *db.AutomationRepository
	var unsubscribe func()
	// room_post reaches rooms only when they run; automation.NewRuntime keeps
	// the script valid and fails the call instead.
	var poster automation.RoomPoster
	if roomService != nil {
		poster = roomPoster{service: roomService}
	}
	if repository, err := db.OpenAutomationRepository(dbPath); err != nil {
		slog.Error("[Node] automation disabled: open automation repository failed", "error", err)
	} else if runtime, err := automation.NewRuntime(context.Background(), repository, sender, poster); err != nil {
		slog.Error("[Node] automation disabled: load scripts failed", "error", err)
		_ = repository.Close()
	} else {
		automationRuntime = runtime
		automationRepository = repository
		unsubscribe = manager.SubscribeGroupText(runtime.Handle)
		slog.Info("[Node] Starlark automation runtime enabled")
	}

	node := &Node{
		manager:              manager,
		radio:                radio,
		scheduler:            scheduler,
		sender:               sender,
		automation:           automationRuntime,
		automationRepository: automationRepository,
		rooms:                roomService,
		roomRepository:       roomRepository,
		unsubscribePackets:   unsubscribePackets,
		unsubscribe:          unsubscribe,
		configPath:           configPath,
		radioState:           radioState,
	}
	node.startMonitor(cfg, persistence, len(roomConfigs), probes.signalReport)
	return node, nil
}

// radioProbes is what a settings apply observes: the confirmed echo and the
// readings the startup and probe paths log.
type radioProbes struct {
	verified        *kiss.RadioConfig
	rssi            int8
	noiseFloor      int16
	noiseCalibrated bool
	// signalReport is nil when the modem's answer was unknown, else whether
	// signal reporting ended up enabled.
	signalReport *bool
}

func configureRadio(ctx context.Context, radio *kiss.RadioClient, cfg *models.KamConfig) (radioProbes, error) {
	probes := radioProbes{}
	if err := radio.Ping(ctx); err != nil {
		return probes, fmt.Errorf("no KISS modem on %q: %w", cfg.Radio.Port, err)
	}
	version, err := radio.GetVersion(ctx)
	if err != nil {
		slog.Warn("radio firmware version unavailable", "error", err)
	}
	deviceName, err := radio.DeviceName(ctx)
	if err != nil {
		slog.Warn("radio device name unavailable", "error", err)
	}
	probes, err = applyRadioSettings(ctx, radio, cfg)
	if err != nil {
		return probes, err
	}
	slog.Info("radio startup probe complete", "device", deviceName, "firmware_version", version.Version,
		"frequency_hz", probes.verified.Frequency, "bandwidth_hz", probes.verified.Bandwidth,
		"spreading_factor", probes.verified.SpreadingFactor, "coding_rate", probes.verified.CodingRate,
		"current_rssi_dbm", probes.rssi, "noise_floor_dbm", probes.noiseFloor, "noise_floor_calibrated", probes.noiseCalibrated)
	return probes, nil
}

// openRooms opens the room repository, loads (or creates) each room's
// identity, and builds the service. It does not start it. Public keys are
// logged so the operator can import the rooms into a client; private keys never
// are.
func openRooms(dbPath string, meshCfg models.MeshConfig, rooms []models.RoomConfig, transmitter room.Transmitter) (*room.Service, *db.RoomRepository, error) {
	repository, err := db.OpenRoomRepository(dbPath)
	if err != nil {
		return nil, nil, fmt.Errorf("open room repository: %w", err)
	}
	keyDir := room.KeyDir(dbPath)
	identities, err := room.LoadIdentities(keyDir, rooms, crand.Reader)
	if err != nil {
		_ = repository.Close()
		return nil, nil, err
	}
	specs := make([]room.Spec, 0, len(rooms))
	for i, cfg := range rooms {
		pub := identities[i].Identity.PublicKey()
		slog.Info("[Node] room identity", "room", cfg.ID, "name", cfg.Name, "public_key", hex.EncodeToString(pub[:]),
			"hash", fmt.Sprintf("%02x", identities[i].Identity.Hash()), "created", identities[i].Created, "key_file", room.IdentityPath(keyDir, cfg.ID))
		specs = append(specs, room.Spec{Config: cfg, Identity: identities[i].Identity})
	}
	service, err := room.NewService(room.Options{Rooms: specs, Mesh: meshCfg, Store: repository, Transmitter: transmitter})
	if err != nil {
		_ = repository.Close()
		return nil, nil, err
	}
	return service, repository, nil
}

func applyRadioSettings(ctx context.Context, radio *kiss.RadioClient, cfg *models.KamConfig) (radioProbes, error) {
	probes := radioProbes{}
	wanted := kiss.RadioConfig{
		Frequency: cfg.Radio.FrequencyHz, Bandwidth: cfg.Radio.BandwidthHz,
		SpreadingFactor: cfg.Radio.SpreadingFactor, CodingRate: cfg.Radio.CodingRate,
	}
	if err := radio.SetRadio(ctx, wanted); err != nil {
		return probes, fmt.Errorf("set radio config: %w", err)
	}
	if cfg.Radio.TxPowerDBm != nil {
		if err := radio.SetTxPower(ctx, *cfg.Radio.TxPowerDBm); err != nil {
			slog.Warn("radio TX power was not configured", "error", err)
		}
	}
	enabled, err := radio.SetSignalReport(ctx, true)
	if err != nil {
		slog.Warn("radio signal reporting could not be enabled", "error", err)
	} else if !enabled {
		slog.Warn("radio did not enable signal reporting")
	}
	if err == nil {
		probes.signalReport = &enabled
	}
	if cfg.Radio.CSMA != nil {
		csma := cfg.Radio.CSMA
		if csma.FullDuplex {
			slog.Warn("radio full_duplex bypasses CSMA; use only with suitable hardware")
		}
		if err := radio.SetCSMA(ctx, kiss.CSMAConfig{
			TxDelayMS: csma.TxDelayMS, Persistence: uint8(csma.Persistence),
			SlotTimeMS: csma.SlotTimeMS, FullDuplex: csma.FullDuplex,
		}); err != nil {
			slog.Warn("radio CSMA settings were not configured", "error", err)
		}
	}
	verified, err := radio.GetRadio(ctx)
	if err != nil {
		return probes, fmt.Errorf("read radio config echo: %w", err)
	}
	if verified != wanted {
		return probes, fmt.Errorf("radio config echo mismatch: got %+v, want %+v", verified, wanted)
	}
	probes.verified = &verified
	probes.rssi, err = radio.GetCurrentRSSI(ctx)
	if err != nil {
		slog.Warn("radio RSSI probe unavailable", "error", err)
	}
	probes.noiseFloor, probes.noiseCalibrated, err = radio.NoiseFloor(ctx)
	if err != nil {
		slog.Warn("radio noise-floor probe unavailable", "error", err)
	} else if !probes.noiseCalibrated {
		slog.Warn("radio noise floor is not calibrated yet")
	}
	if _, err := radio.AirtimeMillis(ctx, kiss.KISSMaxDataSize); err != nil {
		slog.Warn("radio airtime probe unavailable; using TX gate fallback", "error", err)
	}
	return probes, nil
}

func (n *Node) probeRadio(ctx context.Context) {
	ticker := time.NewTicker(radioProbeInterval)
	defer ticker.Stop()
	consecutiveTimeouts := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// A radio change owns the modem, and an uncertain state (restore
			// failed) must not be silently rewritten (§7.5.1 step 7).
			if n.modem != nil && n.modem.blocked() {
				continue
			}
			probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			if err := n.radio.Ping(probeCtx); err != nil {
				cancel()
				consecutiveTimeouts = countProbeFailure(n.manager, consecutiveTimeouts, err)
				if consecutiveTimeouts >= 3 {
					return
				}
				continue
			}
			actual, err := n.radio.GetRadio(probeCtx)
			cancel()
			if err != nil {
				consecutiveTimeouts = countProbeFailure(n.manager, consecutiveTimeouts, err)
				if consecutiveTimeouts >= 3 {
					return
				}
				continue
			}
			consecutiveTimeouts = 0
			echo := n.radioState.echoValue()
			unchanged := echo != nil && actual == *echo && !isZeroRadioConfig(actual)
			if unchanged {
				continue
			}
			slog.Warn("radio cache changed; reapplying startup settings", "actual", actual)
			applyCtx, cancel := context.WithTimeout(ctx, radioStartupTimeout)
			probes, err := applyRadioSettings(applyCtx, n.radio, &models.KamConfig{Radio: n.radioState.settingsValue()})
			cancel()
			if err != nil {
				n.manager.Fail(fmt.Errorf("reapply radio settings after modem reset: %w", err))
				return
			}
			n.radioState.publish(probes.verified, n.radioState.settingsValue())
		}
	}
}

func countProbeFailure(manager *kiss.Manager, failures int, err error) int {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, kiss.ErrModemUnresponsive) {
		failures++
		slog.Warn("radio health probe timed out", "consecutive_timeouts", failures, "error", err)
		if failures >= 3 {
			manager.Fail(fmt.Errorf("three consecutive radio health probes timed out: %w", err))
		}
		return failures
	}
	slog.Warn("radio health probe failed", "error", err)
	return 0
}

func isZeroRadioConfig(config kiss.RadioConfig) bool {
	return config == (kiss.RadioConfig{})
}

// Run watches the YAML config until ctx is cancelled, then shuts down in
// order: intake stops first, then automation and the room worker (which
// flushes its state), then the transmit scheduler, and last the KISS manager.
// It returns an error only when the modem link was lost.
func (n *Node) Run(ctx context.Context) error {
	watchCtx, stopWatcher := context.WithCancel(ctx)
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		config.WatchConfig(watchCtx, n.configPath, 0, config.BootstrapChannels)
	}()

	healthCtx, stopHealth := context.WithCancel(ctx)
	go n.probeRadio(healthCtx)
	var runErr error
	select {
	case <-ctx.Done():
	case <-n.manager.Done():
		if err := n.manager.Err(); err != nil {
			runErr = fmt.Errorf("KISS modem link lost: %w", err)
		} else {
			runErr = fmt.Errorf("KISS modem link lost")
		}
	}
	stopHealth()
	stopWatcher()
	<-watchDone
	if n.monitorServer != nil {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := n.monitorServer.Shutdown(shutdown); err != nil {
			_ = n.monitorServer.Close()
		}
		cancel()
		if n.modem != nil {
			n.modem.Stop()
		}
	}
	if n.unsubscribe != nil {
		n.unsubscribe()
	}
	if n.unsubscribePackets != nil {
		n.unsubscribePackets()
	}
	if n.automation != nil {
		n.automation.Stop()
	}
	if n.automationRepository != nil {
		if err := n.automationRepository.Close(); err != nil {
			slog.Error("[Node] failed to close automation repository", "error", err)
		}
	}
	if n.rooms != nil {
		n.rooms.Stop()
		stats := n.rooms.Stats()
		slog.Info("[Node] room statistics", "logins_ok", stats.LoginsOK, "logins_failed", stats.LoginsFailed, "posts_accepted", stats.PostsAccepted,
			"pushes_sent", stats.PushesSent, "pushes_acked", stats.PushesAcked, "push_timeouts", stats.PushTimeouts)
	}
	if n.roomRepository != nil {
		if err := n.roomRepository.Close(); err != nil {
			slog.Error("[Node] failed to close room repository", "error", err)
		}
	}
	if n.scheduler != nil {
		n.scheduler.Stop()
	}
	n.manager.Stop()
	return runErr
}

// Sender returns the outbound group-text sender. It is safe to use after New
// returns and until Run has stopped the manager.
func (n *Node) Sender() *send.Sender { return n.sender }

// Radio returns the radio client for direct hardware interaction.
func (n *Node) Radio() *kiss.RadioClient { return n.radio }

// RadioEcho returns the modem's cached echo of the host-set radio values, or
// nil when an echo was never confirmed (before the startup apply succeeds).
func (n *Node) RadioEcho() *kiss.RadioConfig {
	if n.radioState == nil {
		return nil
	}
	return n.radioState.echoValue()
}

// ConfigPath returns the YAML config file this node uses as the single source
// of truth for system configuration.
func (n *Node) ConfigPath() string { return n.configPath }

// UpsertChannel persists a channel to the YAML config file and applies it to
// the parser registry without a restart. It is the runtime path for adding or
// updating system configuration, so new config is written by the application
// into the YAML file when needed. #public is rejected (edit the file
// directly).
func (n *Node) UpsertChannel(channel models.ChannelConfig) error {
	return config.UpsertRuntimeChannelAndApply(n.configPath, channel)
}

// roomPoster adapts the room service to automation.RoomPoster.
type roomPoster struct{ service *room.Service }

func (r roomPoster) Post(ctx context.Context, roomID, text string) error {
	return r.service.AddSystemPost(ctx, roomID, text)
}
