package db

import (
	"context"
	"testing"
)

func TestListMonitorAdvertsNewestHeardFirst(t *testing.T) {
	db := openAdvertTestDB(t)
	records := []AdvertRecord{
		{
			PublicKey: []byte("aaaa"), NodeHash: 0xa1, Nickname: "Alpha",
			Signature: []byte{0}, RawPayload: []byte{0},
			SignatureValid: true, IsRepeater: true,
			HasLocation: true, Latitude: 52.78, Longitude: 6.9,
			HopCount: 2, HopStations: `["aa","bb"]`, Timestamp: 1700000000,
		},
		{
			PublicKey: []byte("bbbb"), NodeHash: 0xb2,
			Signature: []byte{0}, RawPayload: []byte{0},
		},
	}
	if err := UpsertAdvertsBatch(db, records); err != nil {
		t.Fatalf("write adverts: %v", err)
	}
	// Make Alpha the older of the two so ordering is decided by the query, not
	// by insertion order.
	if _, err := db.Exec(`UPDATE adverts SET received_at = unixepoch() - 100 WHERE public_key = ?`, []byte("aaaa")); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	page, err := (&Persistence{db: db}).ListMonitorAdverts(context.Background(), 10)
	if err != nil {
		t.Fatalf("list adverts: %v", err)
	}
	if len(page.Adverts) != 2 || page.Truncated {
		t.Fatalf("page = %+v", page)
	}
	if string(page.Adverts[0].PublicKey) != "bbbb" || page.Adverts[1].NodeHash != 0xa1 {
		t.Fatalf("order = %x, %x", page.Adverts[0].PublicKey, page.Adverts[1].PublicKey)
	}
	alpha := page.Adverts[1]
	if alpha.Nickname == nil || *alpha.Nickname != "Alpha" {
		t.Fatalf("nickname = %v", alpha.Nickname)
	}
	if !alpha.HasLocation || alpha.Latitude == nil || alpha.Longitude == nil {
		t.Fatalf("location missing: %+v", alpha)
	}
	if *alpha.Latitude < 52.77 || *alpha.Latitude > 52.79 || *alpha.Longitude < 6.89 || *alpha.Longitude > 6.91 {
		t.Fatalf("coordinates = %v,%v", *alpha.Latitude, *alpha.Longitude)
	}
	if len(alpha.HopStations) != 2 || alpha.HopStations[0] != "aa" || alpha.HopStations[1] != "bb" {
		t.Fatalf("hop stations = %v", alpha.HopStations)
	}
	if alpha.FirstReceivedAt == nil || alpha.LastReceivedAt.IsZero() || alpha.AdvertTimestamp.IsZero() {
		t.Fatalf("times missing: %+v", alpha)
	}
	if beta := page.Adverts[0]; beta.Nickname != nil || beta.HasLocation || beta.Latitude != nil || beta.Longitude != nil {
		t.Fatalf("placeholder fields leaked: %+v", beta)
	}
	if beta := page.Adverts[0]; beta.HopStations == nil {
		t.Fatal("hop stations must decode to an empty slice, not nil")
	}
}

func TestListMonitorAdvertsTruncatesAndValidatesLimit(t *testing.T) {
	db := openAdvertTestDB(t)
	records := []AdvertRecord{
		{PublicKey: []byte("a"), Signature: []byte{0}, RawPayload: []byte{0}},
		{PublicKey: []byte("b"), Signature: []byte{0}, RawPayload: []byte{0}},
	}
	if err := UpsertAdvertsBatch(db, records); err != nil {
		t.Fatalf("write adverts: %v", err)
	}
	p := &Persistence{db: db}
	page, err := p.ListMonitorAdverts(context.Background(), 1)
	if err != nil {
		t.Fatalf("list adverts: %v", err)
	}
	if len(page.Adverts) != 1 || !page.Truncated {
		t.Fatalf("page = %+v", page)
	}
	for _, limit := range []int{0, -1, 10001} {
		if _, err := p.ListMonitorAdverts(context.Background(), limit); err == nil {
			t.Fatalf("limit %d accepted", limit)
		}
	}
}
