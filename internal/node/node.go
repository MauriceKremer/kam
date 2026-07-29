// Package node owns the lifecycle of a running KAM node: a KISS TNC manager
// wired to SQLite persistence, an outbound group-text sender, and a verified
// radio configuration. Construction (New) and the blocking run loop (Run) are
// the only entry points; flag parsing, logging setup, and signal handling stay
// in the caller.
package node

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"kam.bytecoder.nl/internal/config"
	"kam.bytecoder.nl/internal/db"
	"kam.bytecoder.nl/internal/echo"
	"kam.bytecoder.nl/internal/kiss"
	"kam.bytecoder.nl/internal/kiss/send"
	"kam.bytecoder.nl/internal/models"
)

// radioApplyTimeout bounds the KISS set + readback exchange used to push and
// verify the radio configuration during New.
const radioApplyTimeout = 3 * time.Second

// Node is a running KAM node. A Node is constructed by New and run by Run;
// zero values are not valid.
type Node struct {
	manager *kiss.Manager
	radio   *kiss.RadioClient
	sender  *send.Sender
	echo    *echo.Service

	// RadioConfig is the configuration read back from the hardware after New
	// applied it. Use it to report the live radio state at startup.
	RadioConfig *kiss.RadioConfig
}

// New bootstraps the channel database, opens persistence, opens and starts the
// KISS manager, and pushes + verifies the radio configuration. On success the
// returned Node is ready to Run. On failure any opened resources are torn down
// and a wrapped error is returned.
func New(cfg *models.KamConfig, dbPath string) (*Node, error) {
	if err := config.BootstrapChannels(cfg, dbPath); err != nil {
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

	ctx, cancel := context.WithTimeout(context.Background(), radioApplyTimeout)
	defer cancel()
	verified, err := radio.ApplyRadioConfig(ctx, kiss.RadioConfig{
		Frequency:       cfg.Radio.FrequencyHz,
		Bandwidth:       cfg.Radio.BandwidthHz,
		SpreadingFactor: cfg.Radio.SpreadingFactor,
		CodingRate:      cfg.Radio.CodingRate,
	})
	if err != nil {
		// manager.Stop flushes pending writes and closes the persistence store
		// and serial port, so no separate cleanup is needed.
		manager.Stop()
		return nil, fmt.Errorf("apply radio config: %w", err)
	}

	sender := send.NewForManager(manager)

	// Wire the optional echo liveness service: it subscribes to inbound group
	// texts and replies to mentions of the local node on monitored channels.
	var echoService *echo.Service
	if cfg.Echo.Enabled {
		echoService = echo.New(sender, cfg.NodeName, cfg.Echo.Channels, cfg.Echo.Mention)
		manager.SubscribeGroupText(echoService.Handle)
		slog.Info("[Node] echo service enabled",
			"channels", cfg.Echo.Channels, "mention", cfg.Echo.Mention)
	}

	return &Node{
		manager:     manager,
		radio:       radio,
		sender:      sender,
		echo:        echoService,
		RadioConfig: verified,
	}, nil
}

// Run blocks until ctx is cancelled (e.g. by SIGINT/SIGTERM), then stops the
// echo worker (if any) and the KISS manager and closes persistence. It always
// returns nil. Echo is stopped first so its worker cannot enqueue TX frames
// into a manager whose write loop has already exited.
func (n *Node) Run(ctx context.Context) error {
	<-ctx.Done()
	if n.echo != nil {
		n.echo.Stop()
	}
	n.manager.Stop()
	return nil
}

// Sender returns the outbound group-text sender. It is safe to use after New
// returns and until Run has stopped the manager.
func (n *Node) Sender() *send.Sender { return n.sender }

// Echo returns the optional echo liveness service, or nil if disabled.
func (n *Node) Echo() *echo.Service { return n.echo }

// Radio returns the radio client for direct hardware interaction.
func (n *Node) Radio() *kiss.RadioClient { return n.radio }
