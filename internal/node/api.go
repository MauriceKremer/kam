package node

import (
	"log/slog"
	"time"

	"kam.bytecoder.nl/internal/api"
	"kam.bytecoder.nl/internal/db"
	"kam.bytecoder.nl/internal/events"
	"kam.bytecoder.nl/internal/models"
	"kam.bytecoder.nl/internal/systemstats"
)

func (n *Node) startAPI(cfg *models.KamConfig, persistence *db.Persistence, roomCount int, initialSignalReport *bool) {
	settings := cfg.MonitorSettings()
	if !settings.Enabled {
		return
	}
	if err := settings.Validate(); err != nil {
		slog.Error("local API disabled", "error", err)
		return
	}
	ring := events.NewRing(settings.EventRing)
	if n.rooms != nil {
		n.rooms.SetMonitorRecorder(ring.Record)
	}
	var addresses map[byte]string
	if n.rooms != nil {
		addresses = n.rooms.MonitorAddresses()
	}
	n.manager.SetMonitor(ring, cfg.Radio, addresses)
	started := time.Now().UTC().Truncate(time.Millisecond)
	status := n.statusProvider(cfg, persistence, roomCount, started)
	var scriptSource api.Scripts
	if n.automation != nil {
		scriptSource = apiScripts{runtime: n.automation}
	}
	var channelReader api.ChannelReader
	if persistence != nil {
		channelReader = apiChannels{persistence: persistence, configured: cfg.Channels}
	}
	var roomSource api.RoomSource
	if n.rooms != nil && n.roomRepository != nil && persistence != nil {
		roomSource = apiRooms{service: n.rooms, repository: n.roomRepository, persistence: persistence}
	}
	var modemSource api.ModemSource
	if n.scheduler != nil && n.radio != nil && n.radioState != nil {
		controller := newModemController(n.radio, n.scheduler, n.manager, n.radioState, n.configPath, initialSignalReport)
		controller.Start()
		modemSource = controller
		n.modem = controller
	}
	interval := time.Duration(settings.SystemIntervalSec) * time.Second
	collector := systemstats.NewThrottled(systemstats.Collect, interval, n.dbPath)
	systemSource := func() *api.SystemStatus {
		snapshot := collector.Collect()
		return systemToWire(snapshot)
	}
	server, err := api.Listen(settings.Listen, api.NewHandlerWithResources(apiEvents{ring}, status, scriptSource, channelReader, roomSource, modemSource, systemSource))
	if err != nil {
		slog.Error("local API listener unavailable", "address", settings.Listen, "error", err)
		if n.modem != nil {
			n.modem.Stop()
			n.modem = nil
		}
		return
	}
	n.apiServer = server
	slog.Info("local API listening", "address", settings.Listen)
}
