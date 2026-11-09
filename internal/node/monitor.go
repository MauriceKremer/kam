package node

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"kam.bytecoder.nl/internal/api"
	"kam.bytecoder.nl/internal/db"
	"kam.bytecoder.nl/internal/events"
	"kam.bytecoder.nl/internal/models"
	"kam.bytecoder.nl/internal/room"
	"kam.bytecoder.nl/internal/systemstats"
)

// apiEvents adapts the ring to the transport API's EventsSource seam, mapping
// the ring's Pages into the API's wire shape.
type apiEvents struct{ ring *events.Ring }

func (s apiEvents) Page(after uint64, epoch string, limit int) (api.EventsResponse, error) {
	page, err := s.ring.Page(after, epoch, limit)
	if err != nil {
		return api.EventsResponse{}, err
	}
	return api.EventsResponse{Epoch: page.Epoch, FirstSeq: page.FirstSeq, LastSeq: page.LastSeq, NextAfter: page.NextAfter, Reset: page.Reset, Events: page.Events}, nil
}

func (s apiEvents) Stats() api.RingStats {
	stats := s.ring.Stats()
	return api.RingStats{Epoch: stats.Epoch, Capacity: stats.Capacity, FirstSeq: stats.FirstSeq, LastSeq: stats.LastSeq}
}

// systemToWire maps a systemstats snapshot into the API's wire shape; nil
// groups on the snapshot stay nil on the wire.
func systemToWire(snapshot systemstats.Snapshot) *api.SystemStatus {
	value := &api.SystemStatus{CollectedAt: snapshot.CollectedAt}
	if snapshot.Memory != nil {
		value.Memory = &api.SystemMemory{TotalBytes: snapshot.Memory.TotalBytes, AvailableBytes: snapshot.Memory.AvailableBytes, UsedBytes: snapshot.Memory.UsedBytes, UsedPercent: snapshot.Memory.UsedPercent}
	}
	if snapshot.CPU != nil {
		value.CPU = &api.SystemCPU{Model: snapshot.CPU.Model, Cores: snapshot.CPU.Cores, Load1m: snapshot.CPU.Load1m, Load5m: snapshot.CPU.Load5m, Load15m: snapshot.CPU.Load15m}
	}
	if snapshot.Disk != nil {
		value.Disk = &api.SystemDisk{Path: snapshot.Disk.Path, TotalBytes: snapshot.Disk.TotalBytes, AvailableBytes: snapshot.Disk.AvailableBytes, UsedBytes: snapshot.Disk.UsedBytes, UsedPercent: snapshot.Disk.UsedPercent}
	}
	if snapshot.DB != nil {
		value.Database = &api.SystemDB{Path: snapshot.DB.Path, SizeBytes: snapshot.DB.SizeBytes, WalSizeBytes: snapshot.DB.WalSizeBytes}
	}
	return value
}

type apiChannels struct {
	persistence *db.Persistence
	configured  []models.ChannelConfig
}

func (r apiChannels) Channels(ctx context.Context) ([]api.Channel, error) {
	stored, err := r.persistence.ListMonitorChannels(ctx, r.configured)
	if err != nil {
		return nil, err
	}
	channels := make([]api.Channel, 0, len(stored))
	for _, item := range stored {
		channels = append(channels, api.Channel{Name: item.Name, KeyStrategy: item.KeyStrategy, Enabled: item.Enabled, StoredMessages: item.StoredMessages, LastMessageAt: item.LastMessageAt})
	}
	return channels, nil
}

func (r apiChannels) Messages(ctx context.Context, name string, limit int, beforeID int64, query string) (api.ChannelPage, error) {
	stored, err := r.persistence.ListChannelMessages(ctx, name, limit, beforeID, query)
	if err != nil {
		return api.ChannelPage{}, err
	}
	page := api.ChannelPage{Messages: make([]api.ChannelMessage, 0, len(stored.Messages)), NextBeforeID: stored.NextBeforeID}
	for _, item := range stored.Messages {
		page.Messages = append(page.Messages, api.ChannelMessage{ID: item.ID, ReceivedAt: item.ReceivedAt, Sender: item.Sender, Body: item.Body, HopCount: item.HopCount, SNRDB: item.SNRDB, RSSIDBm: item.RSSIDBm})
	}
	return page, nil
}

type apiRooms struct {
	service     *room.Service
	repository  *db.RoomRepository
	persistence *db.Persistence
}

func (r apiRooms) Rooms(ctx context.Context) ([]api.Room, error) {
	snapshot, err := r.service.MonitorSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]api.Room, 0, len(snapshot))
	for _, room := range snapshot {
		stats, err := r.repository.MonitorStats(ctx, room.ID)
		if err != nil {
			return nil, err
		}
		items = append(items, api.Room{
			ID: room.ID, Name: room.Name, Enabled: room.Enabled, PublicKey: room.PublicKey,
			MaxClients: room.MaxClients, ClientsRemembered: len(room.Clients),
			LastActivityAt: room.LastActivityAt, LastPostAt: stats.LastPostAt, RetainedPosts: stats.RetainedPosts,
			Counters: api.RoomCounters{
				LoginsOK: room.Counters.LoginsOK, LoginsFailed: room.Counters.LoginsFailed, LoginsRateLimited: room.Counters.LoginsRateLimited,
				PostsAccepted: room.Counters.PostsAccepted, PostsRejected: room.Counters.PostsRejected, PushesSent: room.Counters.PushesSent,
				PushesAcked: room.Counters.PushesAcked, PushTimeouts: room.Counters.PushTimeouts,
			},
		})
	}
	return items, nil
}

func (r apiRooms) Clients(ctx context.Context, id string) ([]api.RoomClient, error) {
	snapshot, err := r.service.MonitorSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	for _, room := range snapshot {
		if room.ID != id {
			continue
		}
		clients := make([]api.RoomClient, 0, len(room.Clients))
		for _, client := range room.Clients {
			publicKey, err := hex.DecodeString(client.PublicKey)
			if err != nil {
				return nil, fmt.Errorf("decode room client key: %w", err)
			}
			label, err := r.persistence.AdvertNickname(ctx, publicKey)
			if err != nil {
				return nil, err
			}
			clients = append(clients, api.RoomClient{PublicKey: client.PublicKey, Role: roomRoleName(client.Role), LastActivityAt: client.LastActivityAt, PushState: client.PushState, Label: label})
		}
		sort.Slice(clients, func(i, j int) bool { return clients[i].PublicKey < clients[j].PublicKey })
		return clients, nil
	}
	return nil, fmt.Errorf("room %q not found", id)
}

func (r apiRooms) Posts(ctx context.Context, id string, limit int) ([]api.RoomPost, error) {
	posts, err := r.repository.MonitorPosts(ctx, id, limit)
	if err != nil {
		return nil, err
	}
	items := make([]api.RoomPost, 0, len(posts))
	for _, post := range posts {
		items = append(items, api.RoomPost{ID: post.Timestamp, CreatedAt: post.CreatedAt, AuthorKey: hex.EncodeToString(post.Author), Body: post.Text})
	}
	return items, nil
}

// RemoveClient hands the removal to the room worker and maps the room's
// unknown-client answer to the API's not-found sentinel.
func (r apiRooms) RemoveClient(ctx context.Context, roomID string, publicKey [32]byte) error {
	err := r.service.RemoveClient(ctx, roomID, publicKey)
	if errors.Is(err, room.ErrUnknownClient) {
		return fmt.Errorf("%w: %w", api.ErrRoomClientNotFound, err)
	}
	return err
}

func roomRoleName(role uint8) string {
	switch role {
	case room.RoleAdmin:
		return "admin"
	case room.RoleReadWrite:
		return "read_write"
	case room.RoleReadOnly:
		return "read_only"
	default:
		return "guest"
	}
}

func (n *Node) startMonitor(cfg *models.KamConfig, persistence *db.Persistence, roomCount int, initialSignalReport *bool) {
	if n.rooms == nil {
		roomCount = 0
	}
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
	status := func() api.Status {
		value := api.Status{StartedAt: started, NodeName: cfg.NodeName, Persistence: map[string]api.QueueStatus{}, Rooms: api.RoomStatus{Enabled: n.rooms != nil, Count: roomCount}}
		value.Modem.Link = "up"
		if n.radio != nil {
			value.Modem.DeviceName, value.Modem.FirmwareVersion = n.radio.CachedIdentity()
		}
		if n.manager.Err() != nil {
			value.Modem.Link = "down"
		}
		value.Radio = api.RadioStatus{Source: "modem_echo"}
		var settings models.RadioConfig
		if n.radioState != nil {
			settings = n.radioState.settingsValue()
			if echo := n.radioState.echoValue(); echo != nil {
				value.Radio = api.RadioStatus{FrequencyHz: echo.Frequency, BandwidthHz: echo.Bandwidth, SpreadingFactor: echo.SpreadingFactor, CodingRate: echo.CodingRate, TxPowerDBM: settings.TxPowerDBm, Source: "modem_echo"}
			}
		}
		meshCfg := models.DefaultMeshConfig()
		if cfg.Mesh != nil {
			meshCfg = *cfg.Mesh
		}
		scheduled := n.scheduler.Stats()
		value.DutyCycle = api.DutyStatus{ConfiguredPercent: meshCfg.DutyCyclePercent, UsedMS: scheduled.AirtimeUsed.Milliseconds(), WindowS: 3600}
		value.TX = api.TXStatus{Queued: scheduled.Queued, Submitted: scheduled.Submitted, Sent: scheduled.Sent, Completed: scheduled.Completed, Rejected: scheduled.Rejected, Evicted: scheduled.Evicted, Deferred: scheduled.Deferred, GateTimeouts: scheduled.GateTimeouts}
		if persistence != nil {
			adverts := persistence.AdvertQueue.Metrics()
			texts := persistence.GroupTextQueue.Metrics()
			value.Persistence["adverts"] = api.QueueStatus{Depth: adverts.Depth, Dropped: uint64(adverts.Dropped)}
			value.Persistence["group_texts"] = api.QueueStatus{Depth: texts.Depth, Dropped: uint64(texts.Dropped)}
		}
		value.Automation.Enabled = n.automation != nil
		if n.automation != nil {
			enabled, depth, dropped := n.automation.MonitorStats()
			value.Automation.ScriptsEnabled = &enabled
			value.Automation.QueueDepth = &depth
			value.Automation.EventsDropped = &dropped
		}
		return value
	}
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
	var systemSource func() *api.SystemStatus
	interval := time.Duration(settings.SystemIntervalSec) * time.Second
	collector := systemstats.NewThrottled(systemstats.Collect, interval, n.dbPath)
	systemSource = func() *api.SystemStatus {
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
	n.monitorServer = server
	slog.Info("local API listening", "address", settings.Listen)
}
