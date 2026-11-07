package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"kam.bytecoder.nl/internal/events"
)

func TestStatusSystemFlag(t *testing.T) {
	stamp := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	wal := uint64(4096)
	snapshot := &SystemStatus{
		CollectedAt: stamp,
		Memory:      &SystemMemory{TotalBytes: 100, AvailableBytes: 40, UsedBytes: 60, UsedPercent: 60},
		CPU:         &SystemCPU{Model: nil, Cores: 8},
		Disk:        &SystemDisk{Path: "/", TotalBytes: 1000, AvailableBytes: 500, UsedBytes: 500, UsedPercent: 50},
		Database:    &SystemDB{Path: "kam.db", SizeBytes: 10, WalSizeBytes: &wal},
	}
	handler := NewHandlerWithResources(ringEvents{events.NewRing(16)}, func() Status { return Status{} }, nil, nil, nil, nil, func() *SystemStatus { return snapshot })

	t.Run("no flag leaves the response unchanged", func(t *testing.T) {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", "http://127.0.0.1/v1/status", nil))
		if response.Code != 200 {
			t.Fatalf("status: %d", response.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if _, present := body["system"]; present {
			t.Fatal("system section must be absent without the flag")
		}
	})

	t.Run("system=true serves the section", func(t *testing.T) {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", "http://127.0.0.1/v1/status?system=true", nil))
		if response.Code != 200 {
			t.Fatalf("status: %d", response.Code)
		}
		var body Status
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.System == nil || body.System.Memory == nil || body.System.Database == nil {
			t.Fatalf("system section must be served: %+v", body.System)
		}
		if body.System.CPU != nil && body.System.CPU.Model != nil && *body.System.CPU.Model != "" {
			t.Fatalf("nil model must travel as null: %+v", body.System.CPU)
		}
		if body.System.Database.WalSizeBytes == nil || *body.System.Database.WalSizeBytes != 4096 {
			t.Fatalf("wal size must travel: %+v", body.System.Database)
		}
		if !body.System.CollectedAt.Equal(stamp) {
			t.Fatalf("collected_at must travel: %v", body.System.CollectedAt)
		}
	})

	t.Run("system=1 is the same flag", func(t *testing.T) {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", "http://127.0.0.1/v1/status?system=1", nil))
		if response.Code != 200 {
			t.Fatalf("status: %d", response.Code)
		}
		var body Status
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.System == nil {
			t.Fatal("system=1 must serve the section")
		}
	})

	for _, value := range []string{"yes", "0", "false", "TRUE"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", "http://127.0.0.1/v1/status?system="+value, nil))
		if response.Code != 400 {
			t.Fatalf("system=%s: %d, want 400", value, response.Code)
		}
		if body := response.Body.String(); body != `{"error":"system must be 1 or true"}` {
			t.Fatalf("system=%s: %s", value, body)
		}
	}
}

func TestStatusSystemNilSourceOmitsSection(t *testing.T) {
	handler := NewHandlerWithResources(ringEvents{events.NewRing(16)}, func() Status { return Status{} }, nil, nil, nil, nil, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "http://127.0.0.1/v1/status?system=true", nil))
	if response.Code != 200 {
		t.Fatalf("status: %d", response.Code)
	}
	var body Status
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.System != nil {
		t.Fatalf("a nil system seam must omit the section: %+v", body.System)
	}
}
