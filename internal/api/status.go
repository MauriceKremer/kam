package api

import (
	"net/http"
	"time"
)

type RadioStatus struct {
	FrequencyHz     uint32 `json:"frequency_hz"`
	BandwidthHz     uint32 `json:"bandwidth_hz"`
	SpreadingFactor uint8  `json:"spreading_factor"`
	CodingRate      uint8  `json:"coding_rate"`
	TxPowerDBM      *uint8 `json:"tx_power_dbm"`
	Source          string `json:"source"`
}
type ModemStatus struct {
	Link            string  `json:"link"`
	DeviceName      *string `json:"device_name"`
	FirmwareVersion *uint8  `json:"firmware_version"`
}
type TXStatus struct {
	Queued       int    `json:"queued"`
	Submitted    uint64 `json:"submitted"`
	Sent         uint64 `json:"sent"`
	Completed    uint64 `json:"completed"`
	Rejected     uint64 `json:"rejected"`
	Evicted      uint64 `json:"evicted"`
	Deferred     uint64 `json:"deferred"`
	GateTimeouts uint64 `json:"gate_timeouts"`
}
type DutyStatus struct {
	ConfiguredPercent float64 `json:"configured_percent"`
	UsedMS            int64   `json:"used_ms"`
	WindowS           int     `json:"window_s"`
}
type QueueStatus struct {
	Depth   int    `json:"depth"`
	Dropped uint64 `json:"dropped"`
}
type RoomStatus struct {
	Enabled bool `json:"enabled"`
	Count   int  `json:"count"`
}
type AutomationStatus struct {
	Enabled        bool    `json:"enabled"`
	ScriptsEnabled *int    `json:"scripts_enabled"`
	QueueDepth     *int    `json:"queue_depth"`
	EventsDropped  *uint64 `json:"events_dropped"`
}
type RingStatus struct {
	Capacity int    `json:"capacity"`
	FirstSeq uint64 `json:"first_seq"`
	LastSeq  uint64 `json:"last_seq"`
}
type Status struct {
	APIVersion  int                    `json:"api_version"`
	Epoch       string                 `json:"epoch"`
	Now         time.Time              `json:"now"`
	StartedAt   time.Time              `json:"started_at"`
	NodeName    string                 `json:"node_name"`
	Modem       ModemStatus            `json:"modem"`
	Radio       RadioStatus            `json:"radio"`
	DutyCycle   DutyStatus             `json:"duty_cycle"`
	TX          TXStatus               `json:"tx"`
	Persistence map[string]QueueStatus `json:"persistence"`
	Rooms       RoomStatus             `json:"rooms"`
	Automation  AutomationStatus       `json:"automation"`
	EventRing   RingStatus             `json:"event_ring"`
	System      *SystemStatus          `json:"system,omitempty"`
}

// SystemStatus is the opt-in host snapshot served when a status request asks
// for it via ?system=true. Collection and its throttling live in
// internal/systemstats; the system seam hands in the one snapshot function.
type SystemStatus struct {
	CollectedAt time.Time     `json:"collected_at"`
	Memory      *SystemMemory `json:"memory"`
	CPU         *SystemCPU    `json:"cpu"`
	Disk        *SystemDisk   `json:"disk"`
	Database    *SystemDB     `json:"database"`
}
type SystemMemory struct {
	TotalBytes     uint64  `json:"total_bytes"`
	AvailableBytes uint64  `json:"available_bytes"`
	UsedBytes      uint64  `json:"used_bytes"`
	UsedPercent    float64 `json:"used_percent"`
}
type SystemCPU struct {
	Model   *string  `json:"model"`
	Cores   int      `json:"cores"`
	Load1m  *float64 `json:"load_1m"`
	Load5m  *float64 `json:"load_5m"`
	Load15m *float64 `json:"load_15m"`
}
type SystemDisk struct {
	Path           string  `json:"path"`
	TotalBytes     uint64  `json:"total_bytes"`
	AvailableBytes uint64  `json:"available_bytes"`
	UsedBytes      uint64  `json:"used_bytes"`
	UsedPercent    float64 `json:"used_percent"`
}
type SystemDB struct {
	Path         string  `json:"path"`
	SizeBytes    uint64  `json:"size_bytes"`
	WalSizeBytes *uint64 `json:"wal_size_bytes"`
}

// registerStatusRoutes serves /v1/status. system hands in the throttled
// host-snapshot collector; nil hides the system section even when a request
// asks for it.
func registerStatusRoutes(mux *http.ServeMux, ring EventsSource, status func() Status, system func() *SystemStatus) {
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		value := status()
		stats := ring.Stats()
		value.APIVersion, value.Epoch, value.Now = 1, stats.Epoch, time.Now().UTC().Truncate(time.Millisecond)
		value.EventRing = RingStatus{Capacity: stats.Capacity, FirstSeq: stats.FirstSeq, LastSeq: stats.LastSeq}
		if query.Has("system") {
			switch query.Get("system") {
			case "true", "1":
				if system != nil {
					value.System = system()
				}
			default:
				writeJSON(w, 400, map[string]string{"error": "system must be 1 or true"})
				return
			}
		}
		writeJSON(w, 200, value)
	})
}
