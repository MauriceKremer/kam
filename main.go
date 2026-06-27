package main

import (
	"context"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"kam.bytecoder.nl/internal/config"
	"kam.bytecoder.nl/internal/kiss"
)

func main() {
	level := slog.LevelInfo
	if os.Getenv("DEBUG") == "1" {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	}))
	slog.SetDefault(logger)

	channelsCfg, err := config.LoadChannelsConfig("channels.yml")
	if err != nil {
		log.Fatalf("Could not load channels.yml: %v", err)
	}
	if err := config.BootstrapChannels(channelsCfg, "channels.db"); err != nil {
		log.Fatalf("Could not bootstrap channels from channels.yml: %v", err)
	}

	advertQueue, err := config.NewAdvertQueue("channels.db")
	if err != nil {
		log.Fatalf("Could not open advert queue: %v", err)
	}

	slog.Info("Loaded channels config", "count", len(channelsCfg.Channels))

	// Initialize the KISS manager
	manager, err := kiss.NewManager("/dev/cu.usbserial-0001", 115200)
	if err != nil {
		panic(err)
	}
	manager.AdvertStore = advertQueue

	// Start the KISS manager
	manager.Start()

	// Create a short 3-second deadline specifically for this hardware query
	queryCtx, queryCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer queryCancel()

	err = kiss.NewRadioClient(manager).Set(queryCtx, &kiss.RadioConfig{
		Frequency:       869618000,
		Bandwidth:       62500,
		SpreadingFactor: 7,
		CodingRate:      5,
	})
	if err != nil {
		log.Fatalf("Could not set radio config: %v", err)
	}
	slog.Info("Radio configuration set successfully!")

	slog.Info("Querying Heltec board for radio configuration...")
	radio := kiss.NewRadioClient(manager)
	radioCfg, err := radio.Get(queryCtx)
	if err != nil {
		log.Fatalf("Could not get radio config: %v", err)
	}

	// Success! Print the parsed results
	slog.Info("--- Radio Config ---\n")
	slog.Info("Frequency", "MHz", float64(radioCfg.Frequency)/1000000.0)
	slog.Info("Bandwidth", "kHz", float64(radioCfg.Bandwidth)/1000.0)
	slog.Info("Spreading Factor", "SF", radioCfg.SpreadingFactor)
	slog.Info("Coding Rate", "CR", radioCfg.CodingRate)

	slog.Info("Press Ctrl+C to stop")
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	manager.Stop()
	slog.Info("Stopped")
}
