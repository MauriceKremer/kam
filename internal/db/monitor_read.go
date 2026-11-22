package db

import (
	"context"
	"database/sql"
	"encoding/json"
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

// MonitorAdvert is one stored advert: the latest one heard per public key,
// because the adverts table replaces a reheard node's row instead of appending.
// Times use the reception clock, never the node's advertised clock.
type MonitorAdvert struct {
	PublicKey        []byte
	NodeHash         byte
	Nickname         *string
	IsChatNode       bool
	IsRepeater       bool
	IsRoomServer     bool
	IsSensor         bool
	HasLocation      bool
	Latitude         *float64
	Longitude        *float64
	SignatureValid   bool
	HopCount         int
	HopStations      []string
	TimeDriftSeconds int64
	AdvertTimestamp  time.Time
	FirstReceivedAt  *time.Time
	LastReceivedAt   time.Time
}

type MonitorAdvertPage struct {
	Adverts   []MonitorAdvert
	Truncated bool
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

// ListMonitorAdverts returns stored adverts, most recently heard first. A reheard
// node appears once, with its newest advert, so this is a node inventory, not a
// reception log. limit is capped by the caller; one extra row is read to report
// truncation.
func (p *Persistence) ListMonitorAdverts(ctx context.Context, limit int) (MonitorAdvertPage, error) {
	page := MonitorAdvertPage{Adverts: []MonitorAdvert{}}
	if limit < 1 || limit > 10000 {
		return page, fmt.Errorf("limit must be between 1 and 10000")
	}
	rows, err := p.db.QueryContext(ctx, `SELECT public_key, node_hash, nickname,
  is_chat_node, is_repeater, is_room_server, is_sensor,
  has_location, latitude, longitude, signature_valid, hop_count, hop_stations,
  time_drift_seconds, advert_timestamp, first_received_at, received_at
FROM adverts ORDER BY received_at DESC, id DESC LIMIT ?`, limit+1)
	if err != nil {
		return page, fmt.Errorf("read adverts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var item MonitorAdvert
		var nodeHash, hasLocation int
		var nickname sql.NullString
		var latitude, longitude sql.NullFloat64
		var firstReceived sql.NullInt64
		var advertTimestamp, received int64
		var hopStations string
		if err := rows.Scan(&item.PublicKey, &nodeHash, &nickname,
			&item.IsChatNode, &item.IsRepeater, &item.IsRoomServer, &item.IsSensor,
			&hasLocation, &latitude, &longitude, &item.SignatureValid, &item.HopCount, &hopStations,
			&item.TimeDriftSeconds, &advertTimestamp, &firstReceived, &received); err != nil {
			return page, fmt.Errorf("scan advert: %w", err)
		}
		item.NodeHash = byte(nodeHash)
		if nickname.Valid && nickname.String != "" {
			value := nickname.String
			item.Nickname = &value
		}
		item.HasLocation = hasLocation != 0
		if item.HasLocation && latitude.Valid && longitude.Valid {
			lat, lon := latitude.Float64, longitude.Float64
			item.Latitude, item.Longitude = &lat, &lon
		}
		item.HopStations = []string{}
		if hopStations != "" {
			if err := json.Unmarshal([]byte(hopStations), &item.HopStations); err != nil {
				return page, fmt.Errorf("decode hop stations for %x: %w", item.PublicKey, err)
			}
		}
		item.AdvertTimestamp = time.Unix(advertTimestamp, 0).UTC()
		if firstReceived.Valid {
			stamp := time.Unix(firstReceived.Int64, 0).UTC()
			item.FirstReceivedAt = &stamp
		}
		item.LastReceivedAt = time.Unix(received, 0).UTC()
		page.Adverts = append(page.Adverts, item)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	if len(page.Adverts) > limit {
		page.Adverts = page.Adverts[:limit]
		page.Truncated = true
	}
	return page, nil
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
