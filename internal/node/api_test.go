package node

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kam.bytecoder.nl/internal/api"
	"kam.bytecoder.nl/internal/kiss"
	"kam.bytecoder.nl/internal/models"
)

func TestAPIDisabledOrBindFailureKeepsManagerAlive(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	for _, settings := range []models.MonitorConfig{
		{Enabled: false},
		{Enabled: true, Listen: listener.Addr().String(), EventRing: 256},
		{Enabled: true, Listen: "0.0.0.0:8766", EventRing: 256},
	} {
		manager := &kiss.Manager{}
		manager.TestInit()
		node := &Node{manager: manager}
		node.startAPI(&models.KamConfig{Monitor: &settings}, nil, 0, nil)
		if node.apiServer != nil || manager.Err() != nil {
			t.Fatal("API failure affected radio lifecycle")
		}
		select {
		case <-manager.Done():
			t.Fatal("manager stopped")
		default:
		}
		manager.Stop()
	}
}

func TestAPIStatusWithoutAutomation(t *testing.T) {
	manager := &kiss.Manager{}
	manager.TestInit()
	scheduler := kiss.NewTxScheduler(manager, kiss.TxSchedulerConfig{})
	defer scheduler.Stop()
	defer manager.Stop()
	settings := models.DefaultMonitorConfig()
	settings.Listen = "127.0.0.1:0"
	node := &Node{manager: manager, scheduler: scheduler, radio: kiss.NewRadioClient(manager), radioState: &radioRuntime{echo: &kiss.RadioConfig{Frequency: 868000000, Bandwidth: 125000, SpreadingFactor: 8, CodingRate: 5}}}
	node.startAPI(&models.KamConfig{NodeName: "test", Monitor: &settings, Channels: []models.ChannelConfig{{Name: "#private", KeyHex: "secret-key"}}, Rooms: []models.RoomConfig{{AdminPassword: "secret-password"}}}, nil, 0, nil)
	if node.apiServer == nil {
		t.Fatal("API did not start independently")
	}
	defer node.apiServer.Close()
	response, err := http.Get("http://" + node.apiServer.Addr + "/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var value api.Status
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || value.APIVersion != 1 || value.Epoch == "" || value.Automation.Enabled || value.Radio.Source != "modem_echo" || value.EventRing.Capacity != 8192 {
		t.Fatalf("status: %s", body)
	}
	if strings.Contains(string(body), "secret") {
		t.Fatalf("secret in status: %s", body)
	}
	if value.System != nil {
		t.Fatalf("status without the flag must omit the system section: %+v", value.System)
	}
}

func TestAPIStatusSystemSection(t *testing.T) {
	manager := &kiss.Manager{}
	manager.TestInit()
	scheduler := kiss.NewTxScheduler(manager, kiss.TxSchedulerConfig{})
	defer scheduler.Stop()
	defer manager.Stop()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "kam.db")
	if err := os.WriteFile(dbPath, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := models.DefaultMonitorConfig()
	settings.Listen = "127.0.0.1:0"
	node := &Node{manager: manager, scheduler: scheduler, radio: kiss.NewRadioClient(manager), radioState: &radioRuntime{}, dbPath: dbPath}
	node.startAPI(&models.KamConfig{NodeName: "test", Monitor: &settings}, nil, 0, nil)
	if node.apiServer == nil {
		t.Fatal("API did not start")
	}
	defer node.apiServer.Close()
	response, err := http.Get("http://" + node.apiServer.Addr + "/v1/status?system=true")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var value api.Status
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 {
		t.Fatalf("status: %s", body)
	}
	system := value.System
	if system == nil || system.Memory == nil || system.CPU == nil || system.Disk == nil || system.Database == nil {
		t.Fatalf("system section on this host must be fully populated: %s", body)
	}
	if system.Database.SizeBytes != 10 || system.Database.Path != dbPath {
		t.Fatalf("database stats must report the real file: %+v", system.Database)
	}
	if system.Database.WalSizeBytes != nil {
		t.Fatalf("no wal file exists in the test: %+v", system.Database)
	}
	if system.CollectedAt.After(time.Now()) {
		t.Fatalf("collected_at must be a real timestamp: %v", system.CollectedAt)
	}
	cached, err := http.Get("http://" + node.apiServer.Addr + "/v1/status?system=true")
	if err != nil {
		t.Fatal(err)
	}
	defer cached.Body.Close()
	cachedBody, err := io.ReadAll(cached.Body)
	if err != nil {
		t.Fatal(err)
	}
	var cachedValue api.Status
	if err := json.Unmarshal(cachedBody, &cachedValue); err != nil {
		t.Fatal(err)
	}
	if cachedValue.System == nil || !cachedValue.System.CollectedAt.Equal(system.CollectedAt) {
		t.Fatalf("the throttle must serve the cached snapshot: %s vs %s", body, cachedBody)
	}
}
