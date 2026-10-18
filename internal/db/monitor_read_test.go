package db

import (
	"context"
	"path/filepath"
	"testing"

	"kam.bytecoder.nl/internal/models"
)

func TestMonitorChannelCatalogAndHistory(t *testing.T) {
	p, err := NewPersistence(filepath.Join(t.TempDir(), "monitor.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx := context.Background()
	rows := []struct {
		name, sender, body string
		received           int64
		snr                *int
		rssi               *int
	}{
		{"#emmen", "PA3M", "first trail report", 100, ptrInt(34), ptrInt(-97)},
		{"#emmen", "Scout", "second report", 200, nil, nil},
		{"#emmen", "Other", "literal %_ marker", 250, nil, nil},
		{"#public", "PA3M", "public beacon", 300, ptrInt(9), ptrInt(-110)},
	}
	for _, row := range rows {
		_, err := p.db.Exec(`INSERT INTO group_texts(channel_name,sender,body,txt_type,attempt,message_timestamp,time_drift_seconds,received_at,snr_quarter_db,rssi_dbm) VALUES(?,?,?,0,0,?,?,?,?,?)`, row.name, row.sender, row.body, row.received, 0, row.received, row.snr, row.rssi)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	channels, err := p.ListMonitorChannels(ctx, []models.ChannelConfig{{Name: "#emmen", KeyStrategy: "derived", Enabled: true}, {Name: "#empty", KeyStrategy: "explicit", Enabled: false}})
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != 2 || channels[0].StoredMessages != 3 || channels[0].LastMessageAt == nil || channels[1].StoredMessages != 0 || channels[1].LastMessageAt != nil {
		t.Fatalf("channels: %+v", channels)
	}
	page, err := p.ListChannelMessages(ctx, "#emmen", 1, 0, "report")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 || page.Messages[0].ID != 2 || page.NextBeforeID == nil || *page.NextBeforeID != 2 {
		t.Fatalf("page: %+v", page)
	}
	if page.Messages[0].SNRDB != nil || page.Messages[0].RSSIDBm != nil {
		t.Fatalf("unknown metadata must remain null: %+v", page.Messages[0])
	}
	older, err := p.ListChannelMessages(ctx, "#emmen", 50, *page.NextBeforeID, "")
	if err != nil || len(older.Messages) != 1 || older.Messages[0].ID != 1 || older.Messages[0].SNRDB == nil || *older.Messages[0].SNRDB != 8.5 || older.Messages[0].RSSIDBm == nil || *older.Messages[0].RSSIDBm != -97 {
		t.Fatalf("older: %+v, %v", older, err)
	}
	wildcards, err := p.ListChannelMessages(ctx, "#emmen", 50, 0, "%_")
	if err != nil || len(wildcards.Messages) != 1 || wildcards.Messages[0].ID != 3 {
		t.Fatalf("wildcard characters were not treated literally: %+v, %v", wildcards, err)
	}
}

func ptrInt(v int) *int { return &v }
