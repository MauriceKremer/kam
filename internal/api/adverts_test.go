package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type testAdvertReader struct {
	page     AdvertPage
	err      error
	gotLimit int
}

func (r *testAdvertReader) Adverts(_ context.Context, limit int) (AdvertPage, error) {
	r.gotLimit = limit
	return r.page, r.err
}

func advertHandler(reader AdvertReader) http.Handler {
	return NewHandlerWithResources(newRingSource(8), func() Status { return Status{} }, nil, nil, nil, nil, nil, reader)
}

func readAdverts(t *testing.T, handler http.Handler, path string, code int) (map[string]any, *httptest.ResponseRecorder) {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "http://127.0.0.1"+path, nil))
	if response.Code != code {
		t.Fatalf("%s: got %d: %s", path, response.Code, response.Body)
	}
	var body map[string]any
	if code == http.StatusOK {
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
	}
	return body, response
}

func TestAdvertListShape(t *testing.T) {
	stamp := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	earlier := stamp.Add(-time.Hour)
	nickname := "Canal relay"
	lat, lon := 52.78, 6.9
	reader := &testAdvertReader{page: AdvertPage{
		Adverts: []Advert{{
			PublicKey:        "ed8342bc",
			NodeHash:         "8e",
			Nickname:         &nickname,
			IsRepeater:       true,
			HasLocation:      true,
			Latitude:         &lat,
			Longitude:        &lon,
			SignatureValid:   true,
			HopCount:         2,
			HopStations:      []string{"a1", "b2"},
			TimeDriftSeconds: 12,
			AdvertTimestamp:  stamp,
			FirstReceivedAt:  &earlier,
			LastReceivedAt:   stamp,
		}},
		Truncated: true,
	}}
	body, response := readAdverts(t, advertHandler(reader), "/v1/adverts", http.StatusOK)
	if reader.gotLimit != defaultAdvertLimit {
		t.Fatalf("default limit = %d", reader.gotLimit)
	}
	if body["truncated"] != true {
		t.Fatalf("truncated not reported: %v", body["truncated"])
	}
	items, ok := body["adverts"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("adverts = %v", body["adverts"])
	}
	item := items[0].(map[string]any)
	for field, want := range map[string]any{
		"public_key": "ed8342bc", "node_hash": "8e", "nickname": "Canal relay",
		"is_repeater": true, "is_chat_node": false, "has_location": true,
		"latitude": lat, "longitude": lon, "hop_count": float64(2),
		"signature_valid": true, "time_drift_seconds": float64(12),
		"last_received_at":  "2026-11-22T12:00:00Z",
		"first_received_at": "2026-11-22T11:00:00Z",
	} {
		if got := item[field]; got != want {
			t.Errorf("%s = %v, want %v", field, got, want)
		}
	}
	stations := item["hop_stations"].([]any)
	if len(stations) != 2 || stations[0] != "a1" {
		t.Errorf("hop_stations = %v", item["hop_stations"])
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("cache control = %q", response.Header().Get("Cache-Control"))
	}
}

func TestAdvertListLimit(t *testing.T) {
	reader := &testAdvertReader{page: AdvertPage{}}
	handler := advertHandler(reader)
	readAdverts(t, handler, "/v1/adverts?limit=5", http.StatusOK)
	if reader.gotLimit != 5 {
		t.Fatalf("limit passed to reader = %d", reader.gotLimit)
	}
	for _, path := range []string{"/v1/adverts?limit=0", "/v1/adverts?limit=10001", "/v1/adverts?limit=abc", "/v1/adverts?limit=-1"} {
		readAdverts(t, handler, path, http.StatusBadRequest)
	}
}

func TestAdvertListEmptyAndErrors(t *testing.T) {
	body, _ := readAdverts(t, advertHandler(&testAdvertReader{page: AdvertPage{}}), "/v1/adverts", http.StatusOK)
	items, ok := body["adverts"].([]any)
	if !ok || len(items) != 0 || body["truncated"] != false {
		t.Fatalf("empty page = %v", body)
	}
	readAdverts(t, advertHandler(&testAdvertReader{err: errors.New("db on fire")}), "/v1/adverts", http.StatusInternalServerError)
}

func TestAdvertRouteAbsentWithoutReader(t *testing.T) {
	handler := NewHandlerWithResources(newRingSource(8), func() Status { return Status{} }, nil, nil, nil, nil, nil, nil)
	request := httptest.NewRequest("GET", "http://127.0.0.1/v1/adverts", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("route without reader: %d", response.Code)
	}
}
