package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"kam.bytecoder.nl/internal/models"
)

type MonitorChannel struct {
	Name           string
	KeyStrategy    string
	Enabled        bool
	StoredMessages int64
	LastMessageAt  *time.Time
}

type MonitorChannelMessage struct {
	ID         int64
	ReceivedAt time.Time
	Sender     *string
	Body       *string
	HopCount   int
	SNRDB      *float64
	RSSIDBm    *int
}

type MonitorChannelPage struct {
	Messages     []MonitorChannelMessage
	NextBeforeID *int64
}

// ListMonitorChannels combines public channel configuration with retained message counts.
// It intentionally never reads or returns channel key material.
func (p *Persistence) ListMonitorChannels(ctx context.Context, configured []models.ChannelConfig) ([]MonitorChannel, error) {
	channels := make([]MonitorChannel, 0, len(configured))
	for _, channel := range configured {
		var count int64
		var last sql.NullInt64
		err := p.db.QueryRowContext(ctx, `SELECT COUNT(*), MAX(received_at) FROM group_texts WHERE channel_name = ?`, channel.Name).Scan(&count, &last)
		if err != nil {
			return nil, fmt.Errorf("count channel messages: %w", err)
		}
		item := MonitorChannel{Name: channel.Name, KeyStrategy: channel.KeyStrategy, Enabled: channel.Enabled, StoredMessages: count}
		if last.Valid {
			stamp := time.Unix(last.Int64, 0).UTC()
			item.LastMessageAt = &stamp
		}
		channels = append(channels, item)
	}
	return channels, nil
}

// ListChannelMessages returns the newest matching page. beforeID is exclusive; q is a
// case-insensitive plain substring, never a LIKE pattern.
func (p *Persistence) ListChannelMessages(ctx context.Context, channel string, limit int, beforeID int64, q string) (MonitorChannelPage, error) {
	page := MonitorChannelPage{Messages: []MonitorChannelMessage{}}
	if limit < 1 || limit > 200 {
		return page, fmt.Errorf("limit must be between 1 and 200")
	}
	if len(q) > 200 {
		return page, fmt.Errorf("q must be at most 200 UTF-8 bytes")
	}
	if strings.TrimSpace(channel) == "" {
		return page, fmt.Errorf("channel name is required")
	}
	rows, err := p.db.QueryContext(ctx, `SELECT id, received_at, sender, body, hop_count, snr_quarter_db, rssi_dbm
FROM group_texts
WHERE channel_name=? AND (?=0 OR id<?)
AND (?='' OR instr(lower(coalesce(sender,'') || ' ' || coalesce(body,'')),lower(?))>0)
ORDER BY id DESC LIMIT ?`, channel, beforeID, beforeID, q, q, limit+1)
	if err != nil {
		return page, fmt.Errorf("read channel messages: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var item MonitorChannelMessage
		var sender, body sql.NullString
		var received int64
		var snr sql.NullInt64
		var rssi sql.NullInt64
		if err := rows.Scan(&item.ID, &received, &sender, &body, &item.HopCount, &snr, &rssi); err != nil {
			return page, fmt.Errorf("scan channel message: %w", err)
		}
		item.ReceivedAt = time.Unix(received, 0).UTC()
		if sender.Valid {
			value := sender.String
			item.Sender = &value
		}
		if body.Valid {
			value := body.String
			item.Body = &value
		}
		if snr.Valid {
			value := float64(snr.Int64) / 4
			item.SNRDB = &value
		}
		if rssi.Valid {
			value := int(rssi.Int64)
			item.RSSIDBm = &value
		}
		page.Messages = append(page.Messages, item)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	if len(page.Messages) > limit {
		page.Messages = page.Messages[:limit]
		id := page.Messages[len(page.Messages)-1].ID
		page.NextBeforeID = &id
	}
	return page, nil
}
