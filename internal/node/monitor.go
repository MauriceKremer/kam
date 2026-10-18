package node

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"kam.bytecoder.nl/internal/db"
	"kam.bytecoder.nl/internal/models"
	"kam.bytecoder.nl/internal/monitor"
	"kam.bytecoder.nl/internal/room"
)

type monitorChannels struct {
	persistence *db.Persistence
	configured  []models.ChannelConfig
}

func (r monitorChannels) Channels(ctx context.Context) ([]monitor.Channel, error) {
	stored, err := r.persistence.ListMonitorChannels(ctx, r.configured)
	if err != nil {
		return nil, err
	}
	channels := make([]monitor.Channel, 0, len(stored))
	for _, item := range stored {
		channels = append(channels, monitor.Channel{Name: item.Name, KeyStrategy: item.KeyStrategy, Enabled: item.Enabled, StoredMessages: item.StoredMessages, LastMessageAt: item.LastMessageAt})
	}
	return channels, nil
}

func (r monitorChannels) Messages(ctx context.Context, name string, limit int, beforeID int64, query string) (monitor.ChannelPage, error) {
	stored, err := r.persistence.ListChannelMessages(ctx, name, limit, beforeID, query)
	if err != nil {
		return monitor.ChannelPage{}, err
	}
	page := monitor.ChannelPage{Messages: make([]monitor.ChannelMessage, 0, len(stored.Messages)), NextBeforeID: stored.NextBeforeID}
	for _, item := range stored.Messages {
		page.Messages = append(page.Messages, monitor.ChannelMessage{ID: item.ID, ReceivedAt: item.ReceivedAt, Sender: item.Sender, Body: item.Body, HopCount: item.HopCount, SNRDB: item.SNRDB, RSSIDBm: item.RSSIDBm})
	}
	return page, nil
}

type monitorRooms struct {
	service     *room.Service
	repository  *db.RoomRepository
	persistence *db.Persistence
}

func (r monitorRooms) Rooms(ctx context.Context) ([]monitor.Room, error) {
	snapshot, err := r.service.MonitorSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]monitor.Room, 0, len(snapshot))
	for _, room := range snapshot {
		stats, err := r.repository.MonitorStats(ctx, room.ID)
		if err != nil {
			return nil, err
		}
		items = append(items, monitor.Room{
			ID: room.ID, Name: room.Name, Enabled: room.Enabled, PublicKey: room.PublicKey,
			MaxClients: room.MaxClients, ClientsRemembered: len(room.Clients),
			LastActivityAt: room.LastActivityAt, LastPostAt: stats.LastPostAt, RetainedPosts: stats.RetainedPosts,
			Counters: monitor.RoomCounters{
				LoginsOK: room.Counters.LoginsOK, LoginsFailed: room.Counters.LoginsFailed, LoginsRateLimited: room.Counters.LoginsRateLimited,
				PostsAccepted: room.Counters.PostsAccepted, PostsRejected: room.Counters.PostsRejected, PushesSent: room.Counters.PushesSent,
				PushesAcked: room.Counters.PushesAcked, PushTimeouts: room.Counters.PushTimeouts,
			},
		})
	}
	return items, nil
}

func (r monitorRooms) Clients(ctx context.Context, id string) ([]monitor.RoomClient, error) {
	snapshot, err := r.service.MonitorSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	for _, room := range snapshot {
		if room.ID != id {
			continue
		}
		clients := make([]monitor.RoomClient, 0, len(room.Clients))
		for _, client := range room.Clients {
			publicKey, err := hex.DecodeString(client.PublicKey)
			if err != nil {
				return nil, fmt.Errorf("decode room client key: %w", err)
			}
			label, err := r.persistence.AdvertNickname(ctx, publicKey)
			if err != nil {
				return nil, err
			}
			clients = append(clients, monitor.RoomClient{PublicKey: client.PublicKey, Role: roomRoleName(client.Role), LastActivityAt: client.LastActivityAt, PushState: client.PushState, Label: label})
		}
		sort.Slice(clients, func(i, j int) bool { return clients[i].PublicKey < clients[j].PublicKey })
		return clients, nil
	}
	return nil, fmt.Errorf("room %q not found", id)
}

func (r monitorRooms) Posts(ctx context.Context, id string, limit int) ([]monitor.RoomPost, error) {
	posts, err := r.repository.MonitorPosts(ctx, id, limit)
	if err != nil {
		return nil, err
	}
	items := make([]monitor.RoomPost, 0, len(posts))
	for _, post := range posts {
		items = append(items, monitor.RoomPost{ID: post.Timestamp, CreatedAt: post.CreatedAt, AuthorKey: hex.EncodeToString(post.Author), Body: post.Text})
	}
	return items, nil
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

func (n *Node) startMonitor(cfg *models.KamConfig, persistence *db.Persistence, roomCount int) {
	if n.rooms == nil {
		roomCount = 0
	}
	settings := cfg.MonitorSettings()
	if !settings.Enabled {
		return
	}
	if err := settings.Validate(); err != nil {
		slog.Error("monitor disabled", "error", err)
		return
	}
	ring := monitor.NewRing(settings.EventRing)
	if n.rooms != nil {
		n.rooms.SetMonitorRecorder(ring.Record)
	}
	var addresses map[byte]string
	if n.rooms != nil {
		addresses = n.rooms.MonitorAddresses()
	}
	n.manager.SetMonitor(ring, cfg.Radio, addresses)
	started := time.Now().UTC().Truncate(time.Millisecond)
	status := func() monitor.Status {
		value := monitor.Status{StartedAt: started, NodeName: cfg.NodeName, Persistence: map[string]monitor.QueueStatus{}, Rooms: monitor.RoomStatus{Enabled: n.rooms != nil, Count: roomCount}}
		value.Modem.Link = "up"
		value.Modem.DeviceName, value.Modem.FirmwareVersion = n.radio.CachedIdentity()
		if n.manager.Err() != nil {
			value.Modem.Link = "down"
		}
		value.Radio = monitor.RadioStatus{FrequencyHz: n.RadioConfig.Frequency, BandwidthHz: n.RadioConfig.Bandwidth, SpreadingFactor: n.RadioConfig.SpreadingFactor, CodingRate: n.RadioConfig.CodingRate, TxPowerDBM: cfg.Radio.TxPowerDBm, Source: "modem_echo"}
		meshCfg := models.DefaultMeshConfig()
		if cfg.Mesh != nil {
			meshCfg = *cfg.Mesh
		}
		scheduled := n.scheduler.Stats()
		value.DutyCycle = monitor.DutyStatus{ConfiguredPercent: meshCfg.DutyCyclePercent, UsedMS: scheduled.AirtimeUsed.Milliseconds(), WindowS: 3600}
		value.TX = monitor.TXStatus{Queued: scheduled.Queued, Submitted: scheduled.Submitted, Sent: scheduled.Sent, Completed: scheduled.Completed, Rejected: scheduled.Rejected, Evicted: scheduled.Evicted, Deferred: scheduled.Deferred, GateTimeouts: scheduled.GateTimeouts}
		if persistence != nil {
			adverts := persistence.AdvertQueue.Metrics()
			texts := persistence.GroupTextQueue.Metrics()
			value.Persistence["adverts"] = monitor.QueueStatus{Depth: adverts.Depth, Dropped: uint64(adverts.Dropped)}
			value.Persistence["group_texts"] = monitor.QueueStatus{Depth: texts.Depth, Dropped: uint64(texts.Dropped)}
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
	var channelReader monitor.ChannelReader
	if persistence != nil {
		channelReader = monitorChannels{persistence: persistence, configured: cfg.Channels}
	}
	var roomReader monitor.RoomReader
	if n.rooms != nil && n.roomRepository != nil && persistence != nil {
		roomReader = monitorRooms{service: n.rooms, repository: n.roomRepository, persistence: persistence}
	}
	server, err := monitor.Listen(settings.Listen, monitor.NewHandlerWithResources(ring, status, channelReader, roomReader))
	if err != nil {
		slog.Error("monitor listener unavailable", "address", settings.Listen, "error", err)
		return
	}
	n.monitorServer = server
	slog.Info("monitor API listening", "address", settings.Listen)
}
