package node

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"kam.bytecoder.nl/internal/kiss"
	"kam.bytecoder.nl/internal/models"
	"kam.bytecoder.nl/internal/monitor"
)

func TestMonitorDisabledOrBindFailureKeepsManagerAlive(t *testing.T) {
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
		node.startMonitor(&models.KamConfig{Monitor: &settings}, nil, 0)
		if node.monitorServer != nil || manager.Err() != nil {
			t.Fatal("monitor failure affected radio lifecycle")
		}
		select {
		case <-manager.Done():
			t.Fatal("manager stopped")
		default:
		}
		manager.Stop()
	}
}

func TestMonitorStatusWithoutAutomation(t *testing.T) {
	manager := &kiss.Manager{}
	manager.TestInit()
	scheduler := kiss.NewTxScheduler(manager, kiss.TxSchedulerConfig{})
	defer scheduler.Stop()
	defer manager.Stop()
	settings := models.DefaultMonitorConfig()
	settings.Listen = "127.0.0.1:0"
	node := &Node{manager: manager, scheduler: scheduler, radio: kiss.NewRadioClient(manager), RadioConfig: &kiss.RadioConfig{Frequency: 868000000, Bandwidth: 125000, SpreadingFactor: 8, CodingRate: 5}}
	node.startMonitor(&models.KamConfig{NodeName: "test", Monitor: &settings, Channels: []models.ChannelConfig{{Name: "#private", KeyHex: "secret-key"}}, Rooms: []models.RoomConfig{{AdminPassword: "secret-password"}}}, nil, 0)
	if node.monitorServer == nil {
		t.Fatal("monitor did not start independently")
	}
	defer node.monitorServer.Close()
	response, err := http.Get("http://" + node.monitorServer.Addr + "/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var value monitor.Status
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || value.APIVersion != 1 || value.Epoch == "" || value.Automation.Enabled || value.Radio.Source != "modem_echo" || value.EventRing.Capacity != 8192 {
		t.Fatalf("status: %s", body)
	}
	if strings.Contains(string(body), "secret") {
		t.Fatalf("secret in status: %s", body)
	}
}
