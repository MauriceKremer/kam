package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"kam.bytecoder.nl/internal/config"
	"kam.bytecoder.nl/internal/db"
	"kam.bytecoder.nl/internal/kiss"
	"kam.bytecoder.nl/internal/kiss/send"
)

func main() {
	configPath := flag.String("config", "kam.yml", "path to kam.yml configuration file")
	dbPath := flag.String("db", "", "SQLite database path (overrides kam.yml db_path; default kam.db)")
	portOverride := flag.String("port", "", "override radio serial port from config (e.g. /dev/ttyUSB0)")
	flag.Parse()

	level := slog.LevelInfo
	if os.Getenv("DEBUG") == "1" {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	}))
	slog.SetDefault(logger)

	kamCfg, err := config.LoadKamConfig(*configPath)
	if err != nil {
		slog.Error("Could not load config", "path", *configPath, "error", err)
		os.Exit(1)
	}
	if *portOverride != "" {
		kamCfg.Radio.Port = *portOverride
	}

	// Resolve DB path: -db flag > kam.yml db_path > default "kam.db".
	resolvedDBPath := *dbPath
	if resolvedDBPath == "" {
		if kamCfg.DBPath != "" {
			resolvedDBPath = kamCfg.DBPath
		} else {
			resolvedDBPath = "kam.db"
		}
	}

	if err := config.BootstrapChannels(kamCfg, resolvedDBPath); err != nil {
		slog.Error("Could not bootstrap channels", "error", err)
		os.Exit(1)
	}

	persistence, err := db.NewPersistence(resolvedDBPath)
	if err != nil {
		slog.Error("Could not open persistence", "path", resolvedDBPath, "error", err)
		os.Exit(1)
	}

	slog.Info("Loaded channels config", "count", len(kamCfg.Channels))

	// Initialize the KISS manager
	manager, err := kiss.NewManager(kamCfg.Radio.Port, kamCfg.Radio.Baud)
	if err != nil {
		slog.Error("Could not open serial port",
			"port", kamCfg.Radio.Port,
			"baud", kamCfg.Radio.Baud,
			"error", err,
			"hint", "connect a MeshCore-compatible radio or pass -port /dev/...",
		)
		os.Exit(1)
	}
	manager.Store = persistence
	manager.NodeName = kamCfg.NodeName

	// Start the KISS manager
	manager.Start()

	radio := kiss.NewRadioClient(manager)

	// Push radio configuration to the hardware
	setCtx, setCancel := context.WithTimeout(context.Background(), 3*time.Second)
	err = radio.Set(setCtx, &kiss.RadioConfig{
		Frequency:       kamCfg.Radio.FrequencyHz,
		Bandwidth:       kamCfg.Radio.BandwidthHz,
		SpreadingFactor: kamCfg.Radio.SpreadingFactor,
		CodingRate:      kamCfg.Radio.CodingRate,
	})
	setCancel()
	if err != nil {
		slog.Error("Could not set radio config", "error", err)
		os.Exit(1)
	}
	slog.Info("Radio configuration set successfully")

	// Query the board back to confirm
	getCtx, getCancel := context.WithTimeout(context.Background(), 3*time.Second)
	radioCfg, err := radio.Get(getCtx)
	getCancel()
	if err != nil {
		slog.Error("Could not get radio config", "error", err)
		os.Exit(1)
	}

	slog.Info("--- Radio Config ---")
	slog.Info("Frequency", "MHz", fmt.Sprintf("%.3f", float64(radioCfg.Frequency)/1000000.0))
	slog.Info("Bandwidth", "kHz", fmt.Sprintf("%.1f", float64(radioCfg.Bandwidth)/1000.0))
	slog.Info("Spreading Factor", "SF", radioCfg.SpreadingFactor)
	slog.Info("Coding Rate", "CR", radioCfg.CodingRate)

	slog.Info("Press Ctrl+C to stop")

	// Outbound group-text sender (Phase 1 placeholder)
	_ = send.NewForManager(manager)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	manager.Stop()
	slog.Info("Stopped")
}
