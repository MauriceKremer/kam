// Command kam is the entry point for the KAM node — a MeshCore-compatible
// LoRa packet gateway. It loads kam.yml, constructs the node (which
// bootstraps the SQLite channel store, opens the KISS TNC serial port, and
// pushes + verifies the radio configuration), and then blocks until
// interrupted.
//
// Usage:
//
//	kam [-config kam.yml] [-db path] [-port /dev/ttyUSB0]
//
// Flags override values from the config file. Set DEBUG=1 for debug logs.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"kam.bytecoder.nl/internal/config"
	"kam.bytecoder.nl/internal/node"
)

// main runs the KAM node lifecycle: parse flags, configure logging, load
// kam.yml, construct the node, report the verified radio configuration, then
// block on SIGINT/SIGTERM until the node stops.
func main() {
	configPath := flag.String("config", "kam.yml", "path to kam.yml configuration file")
	dbPath := flag.String("db", "", "SQLite database path (overrides kam.yml db_path; default kam.db)")
	portOverride := flag.String("port", "", "override radio serial port from config (e.g. /dev/ttyUSB0)")
	flag.Parse()

	level := slog.LevelInfo
	if os.Getenv("DEBUG") == "1" {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	})))

	kamCfg, err := config.LoadKamConfig(*configPath)
	if err != nil {
		slog.Error("Could not load config", "path", *configPath, "error", err)
		os.Exit(1)
	}
	if *portOverride != "" {
		kamCfg.Radio.Port = *portOverride
	}
	resolvedDBPath := config.ResolveDBPath(*dbPath, kamCfg)

	slog.Info("Loaded channels config", "count", len(kamCfg.Channels))

	n, err := node.New(kamCfg, resolvedDBPath)
	if err != nil {
		slog.Error("Could not start node", "error", err,
			"hint", "connect a MeshCore-compatible radio or pass -port /dev/...",
		)
		os.Exit(1)
	}

	slog.Info("Radio configuration set successfully")
	slog.Info("--- Radio Config ---")
	slog.Info("Frequency", "MHz", fmt.Sprintf("%.3f", float64(n.RadioConfig.Frequency)/1000000.0))
	slog.Info("Bandwidth", "kHz", fmt.Sprintf("%.1f", float64(n.RadioConfig.Bandwidth)/1000.0))
	slog.Info("Spreading Factor", "SF", n.RadioConfig.SpreadingFactor)
	slog.Info("Coding Rate", "CR", n.RadioConfig.CodingRate)

	slog.Info("Press Ctrl+C to stop")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	n.Run(ctx)

	slog.Info("Stopped")
}
