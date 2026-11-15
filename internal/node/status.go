package node

import (
	"time"

	"kam.bytecoder.nl/internal/api"
	"kam.bytecoder.nl/internal/db"
	"kam.bytecoder.nl/internal/models"
	"kam.bytecoder.nl/internal/systemstats"
)

func (n *Node) statusProvider(cfg *models.KamConfig, persistence *db.Persistence, roomCount int, started time.Time) func() api.Status {
	if n.rooms == nil {
		roomCount = 0
	}
	return func() api.Status {
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
