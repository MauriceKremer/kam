package models

import (
	"fmt"
	"net"
	"net/netip"
	"time"

	"gopkg.in/yaml.v3"
)

type MonitorConfig struct {
	Enabled           bool   `yaml:"enabled"`
	Listen            string `yaml:"listen"`
	EventRing         int    `yaml:"event_ring"`
	SystemIntervalSec int    `yaml:"system_interval_sec"`
}

func DefaultMonitorConfig() MonitorConfig {
	return MonitorConfig{Enabled: true, Listen: "127.0.0.1:8766", EventRing: 8192, SystemIntervalSec: 5}
}

func (c *MonitorConfig) UnmarshalYAML(node *yaml.Node) error {
	type plain MonitorConfig
	value := plain(DefaultMonitorConfig())
	if err := node.Decode(&value); err != nil {
		return err
	}
	*c = MonitorConfig(value)
	return nil
}

func (c *KamConfig) MonitorSettings() MonitorConfig {
	if c.Monitor == nil {
		return DefaultMonitorConfig()
	}
	return *c.Monitor
}

func (c MonitorConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("monitor.listen must be a loopback host:port: %w", err)
	}
	address, err := netip.ParseAddr(host)
	if err != nil || !address.IsLoopback() {
		return fmt.Errorf("monitor.listen must use a loopback IP address")
	}
	if c.EventRing < 256 || c.EventRing > 65536 {
		return fmt.Errorf("monitor.event_ring must be between 256 and 65536")
	}
	if c.SystemIntervalSec < 0 || c.SystemIntervalSec > 86400 {
		return fmt.Errorf("monitor.system_interval_sec must be between 0 and 86400")
	}
	return nil
}

type MonitorEvent struct {
	Seq         uint64    `json:"seq"`
	At          time.Time `json:"at"`
	Kind        string    `json:"kind"`
	PayloadType string    `json:"payload_type"`
	Route       string    `json:"route"`
	Hops        int       `json:"hops"`
	Len         int       `json:"len"`
	SNRDB       *float64  `json:"snr_db"`
	RSSIDBM     *int      `json:"rssi_dbm"`
	AirtimeMS   *int64    `json:"airtime_ms"`
	Duplicate   bool      `json:"duplicate"`
	DestHash    *string   `json:"dest_hash"`
	Room        *string   `json:"room"`
	Channel     *string   `json:"channel"`
	Name        *string   `json:"name"`
	AdvertRole  *string   `json:"advert_role"`
	OK          *bool     `json:"ok"`
	Origin      string    `json:"origin,omitempty"`
	Event       string    `json:"event,omitempty"`
	Client      string    `json:"client,omitempty"`
	Role        string    `json:"role,omitempty"`
}

type MonitorSink interface {
	Record(MonitorEvent)
}
