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
	apiServer            *http.Server
	monitorServer        *http.Server
	apiCancel            context.CancelFunc
	unsubscribe          func()
	configPath           string
	radioSettings        models.RadioConfig

	// RadioConfig contains the modem's cached echo of the host-set values.
	RadioConfig *kiss.RadioConfig
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
	verified, err := configureRadio(ctx, radio, cfg)
	if err != nil {
		// manager.Stop flushes pending writes and closes the persistence store
		// and serial port, so no separate cleanup is needed.
		manager.Stop()
		return nil, fmt.Errorf("apply radio config: %w", err)
	}

	meshCfg, roomConfigs, problems := cfg.ResolveRooms()
	for _, problem := range problems {
		slog.Error("[Node] room configuration problem", "error", problem)
	}

	// One scheduler owns the radio data path for every producer: group texts
	// from the sender and automation, and everything the rooms transmit.
	scheduler := kiss.NewTxScheduler(manager, kiss.TxSchedulerConfig{
		DutyCyclePercent: meshCfg.DutyCyclePercent,
		Airtime: func(packetBytes int) time.Duration {
			return kiss.LoRaAirtime(verified.SpreadingFactor, verified.Bandwidth, verified.CodingRate, packetBytes)
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
	var apiServer *http.Server
	var apiCancel context.CancelFunc
	var unsubscribe func()
	if repository, err := db.OpenAutomationRepository(dbPath); err != nil {
		slog.Error("[Node] automation disabled: open automation repository failed", "error", err)
	} else if runtime, err := automation.NewRuntime(context.Background(), repository, sender); err != nil {
		slog.Error("[Node] automation disabled: load scripts failed", "error", err)
		_ = repository.Close()
	} else {
		automationRuntime = runtime
		automationRepository = repository
		unsubscribe = manager.SubscribeGroupText(runtime.Handle)
		apiCtx, cancel := context.WithCancel(context.Background())
		if server, err := automation.ServeLocalAPI(apiCtx, runtime); err != nil {
			slog.Error("[Node] Local Management API unavailable", "address", automation.LocalAPIAddress, "error", err)
			cancel()
		} else {
			apiServer = server
			apiCancel = cancel
			slog.Info("[Node] Local Management API listening", "address", automation.LocalAPIAddress)
		}
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
		apiServer:            apiServer,
		apiCancel:            apiCancel,
		unsubscribe:          unsubscribe,
		configPath:           configPath,
		radioSettings:        cfg.Radio,
		RadioConfig:          verified,
	}
	node.startMonitor(cfg, persistence, len(roomConfigs))
	return node, nil
}

func configureRadio(ctx context.Context, radio *kiss.RadioClient, cfg *models.KamConfig) (*kiss.RadioConfig, error) {
	if err := radio.Ping(ctx); err != nil {
		return nil, fmt.Errorf("no KISS modem on %q: %w", cfg.Radio.Port, err)
	}
	version, err := radio.GetVersion(ctx)
	if err != nil {
		slog.Warn("radio firmware version unavailable", "error", err)
	}
	deviceName, err := radio.DeviceName(ctx)
	if err != nil {
		slog.Warn("radio device name unavailable", "error", err)
	}
	verified, rssi, noiseFloor, calibrated, err := applyRadioSettings(ctx, radio, cfg)
	if err != nil {
		return nil, err
	}
	slog.Info("radio startup probe complete", "device", deviceName, "firmware_version", version.Version,
		"frequency_hz", verified.Frequency, "bandwidth_hz", verified.Bandwidth,
		"spreading_factor", verified.SpreadingFactor, "coding_rate", verified.CodingRate,
		"current_rssi_dbm", rssi, "noise_floor_dbm", noiseFloor, "noise_floor_calibrated", calibrated)
	return verified, nil
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

func applyRadioSettings(ctx context.Context, radio *kiss.RadioClient, cfg *models.KamConfig) (*kiss.RadioConfig, int8, int16, bool, error) {
	wanted := kiss.RadioConfig{
		Frequency: cfg.Radio.FrequencyHz, Bandwidth: cfg.Radio.BandwidthHz,
		SpreadingFactor: cfg.Radio.SpreadingFactor, CodingRate: cfg.Radio.CodingRate,
	}
	if err := radio.SetRadio(ctx, wanted); err != nil {
		return nil, 0, 0, false, fmt.Errorf("set radio config: %w", err)
	}
	if cfg.Radio.TxPowerDBm != nil {
		if err := radio.SetTxPower(ctx, *cfg.Radio.TxPowerDBm); err != nil {
			slog.Warn("radio TX power was not configured", "error", err)
		}
	}
	if enabled, err := radio.SetSignalReport(ctx, true); err != nil {
		slog.Warn("radio signal reporting could not be enabled", "error", err)
	} else if !enabled {
		slog.Warn("radio did not enable signal reporting")
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
		return nil, 0, 0, false, fmt.Errorf("read radio config echo: %w", err)
	}
	if verified != wanted {
		return nil, 0, 0, false, fmt.Errorf("radio config echo mismatch: got %+v, want %+v", verified, wanted)
	}
	rssi, err := radio.GetCurrentRSSI(ctx)
	if err != nil {
		slog.Warn("radio RSSI probe unavailable", "error", err)
	}
	noiseFloor, calibrated, err := radio.NoiseFloor(ctx)
	if err != nil {
		slog.Warn("radio noise-floor probe unavailable", "error", err)
	} else if !calibrated {
		slog.Warn("radio noise floor is not calibrated yet")
	}
	if _, err := radio.AirtimeMillis(ctx, kiss.KISSMaxDataSize); err != nil {
		slog.Warn("radio airtime probe unavailable; using TX gate fallback", "error", err)
	}
	return &verified, rssi, noiseFloor, calibrated, nil
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
			if isZeroRadioConfig(actual) || actual != *n.RadioConfig {
				slog.Warn("radio cache changed; reapplying startup settings", "actual", actual)
				applyCtx, cancel := context.WithTimeout(ctx, radioStartupTimeout)
				_, _, _, _, err := applyRadioSettings(applyCtx, n.radio, &models.KamConfig{Radio: n.radioSettings})
				cancel()
				if err != nil {
					n.manager.Fail(fmt.Errorf("reapply radio settings after modem reset: %w", err))
					return
				}
			}
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
	}
	if n.unsubscribe != nil {
		n.unsubscribe()
	}
	if n.unsubscribePackets != nil {
		n.unsubscribePackets()
	}
	if n.apiServer != nil {
		shutdownCtx, cancelAPI := context.WithTimeout(context.Background(), 5*time.Second)
		if err := n.apiServer.Shutdown(shutdownCtx); err != nil {
			slog.Error("[Node] Local Management API shutdown failed", "error", err)
			_ = n.apiServer.Close()
		}
		cancelAPI()
	}
	if n.apiCancel != nil {
		n.apiCancel()
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
