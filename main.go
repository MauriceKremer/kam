// Command kam is the entry point for the KAM node — a MeshCore-compatible
// LoRa packet gateway. It loads kam.yml, constructs the node (which
// bootstraps the SQLite channel store, opens the KISS TNC serial port, and
// pushes + verifies the radio configuration), and then blocks until
// interrupted.
//
// Usage:
//
//	kam [-config kam.yml] [-db path] [-port /dev/ttyUSB0]
//	kam channel upsert -name '#ops' -key-strategy derived
//	kam room show [-config kam.yml] [-db path]
//
// Flags override values from the config file. Set DEBUG=1 for debug logs.
package main

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"kam.bytecoder.nl/internal/config"
	"kam.bytecoder.nl/internal/models"
	"kam.bytecoder.nl/internal/node"
	"kam.bytecoder.nl/internal/room"
)

// main runs the KAM node lifecycle: parse flags, configure logging, load
// kam.yml, construct the node, report the verified radio configuration, then
// block on SIGINT/SIGTERM until the node stops.
func main() {
	if len(os.Args) > 1 && os.Args[1] == "channel" {
		if err := runChannelCommand(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "room" {
		if err := runRoomCommand(os.Args[2:], os.Stdout, os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		return
	}

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

	n, err := node.New(kamCfg, resolvedDBPath, *configPath)
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

// runRoomCommand implements "kam room show": it prints each configured room's
// public key and signed advert so the room can be imported into a client. A
// room's identity key is created on first use, exactly as at node start.
func runRoomCommand(args []string, stdout, stderr io.Writer) error {
	const usage = "usage: kam room show [-config kam.yml] [-db path]"
	if len(args) == 0 || args[0] != "show" {
		return fmt.Errorf("%s", usage)
	}
	flags := flag.NewFlagSet("room show", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "kam.yml", "path to kam.yml configuration file")
	dbPath := flags.String("db", "", "SQLite database path (identity keys live in a rooms directory next to it)")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %v\n%s", flags.Args(), usage)
	}

	cfg, err := config.LoadKamConfig(*configPath)
	if err != nil {
		return err
	}
	meshCfg, rooms, problems := cfg.ResolveRooms()
	for _, problem := range problems {
		fmt.Fprintf(stderr, "warning: %v\n", problem)
	}
	if len(rooms) == 0 {
		return fmt.Errorf("no enabled rooms in %s; add a rooms: list (see README)", *configPath)
	}
	keyDir := room.KeyDir(config.ResolveDBPath(*dbPath, cfg))
	identities, err := room.LoadIdentities(keyDir, rooms, crand.Reader)
	if err != nil {
		return err
	}

	for i, roomCfg := range rooms {
		id := identities[i].Identity
		pub := id.PublicKey()
		pubHex := hex.EncodeToString(pub[:])
		advert, err := room.BuildAdvert(id, roomCfg, meshCfg, uint32(time.Now().Unix()), true)
		if err != nil {
			return fmt.Errorf("room %s: %w", roomCfg.ID, err)
		}
		raw, err := advert.Encode()
		if err != nil {
			return fmt.Errorf("room %s: %w", roomCfg.ID, err)
		}
		contact := url.Values{"name": {roomCfg.Name}, "public_key": {pubHex}, "type": {"3"}}
		status := "existing"
		if identities[i].Created {
			status = "created now"
		}
		fmt.Fprintf(stdout, "Room %s (%q)\n", roomCfg.ID, roomCfg.Name)
		fmt.Fprintf(stdout, "  public key : %s\n", pubHex)
		fmt.Fprintf(stdout, "  hash       : %02x\n", id.Hash())
		fmt.Fprintf(stdout, "  key file   : %s (%s; back it up, it is the room's address)\n", room.IdentityPath(keyDir, roomCfg.ID), status)
		fmt.Fprintf(stdout, "  contact URI: meshcore://contact/add?%s\n", contact.Encode())
		fmt.Fprintf(stdout, "  advert hex : %s\n", hex.EncodeToString(raw))
		fmt.Fprintf(stdout, "  (the advert is signed now; clients ignore an advert that is not newer than the last one they stored)\n\n")
	}
	return nil
}

func runChannelCommand(args []string) error {
	if len(args) == 0 || args[0] != "upsert" {
		return fmt.Errorf("usage: kam channel upsert -config kam.yml -name '#channel' -key-strategy derived|explicit [-key-hex hex] [-enabled=false]")
	}

	flags := flag.NewFlagSet("channel upsert", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configPath := flags.String("config", "kam.yml", "path to kam.yml configuration file")
	name := flags.String("name", "", "channel name")
	keyStrategy := flags.String("key-strategy", "", "channel key strategy: derived or explicit")
	keyHex := flags.String("key-hex", "", "explicit channel key in hex")
	enabled := flags.Bool("enabled", true, "whether the channel is enabled")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	if *name == "" || *keyStrategy == "" {
		return fmt.Errorf("-name and -key-strategy are required")
	}

	return config.UpsertChannelInConfigFile(*configPath, models.ChannelConfig{
		Name:        *name,
		KeyStrategy: *keyStrategy,
		KeyHex:      *keyHex,
		Enabled:     *enabled,
	})
}
