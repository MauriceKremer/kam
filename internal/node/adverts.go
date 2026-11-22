package node

import (
	"context"
	"encoding/hex"

	"kam.bytecoder.nl/internal/api"
	"kam.bytecoder.nl/internal/db"
)

type apiAdverts struct {
	persistence *db.Persistence
}

func (r apiAdverts) Adverts(ctx context.Context, limit int) (api.AdvertPage, error) {
	stored, err := r.persistence.ListMonitorAdverts(ctx, limit)
	if err != nil {
		return api.AdvertPage{}, err
	}
	page := api.AdvertPage{Adverts: make([]api.Advert, 0, len(stored.Adverts)), Truncated: stored.Truncated}
	for _, item := range stored.Adverts {
		page.Adverts = append(page.Adverts, api.Advert{
			PublicKey:        hex.EncodeToString(item.PublicKey),
			NodeHash:         hex.EncodeToString([]byte{item.NodeHash}),
			Nickname:         item.Nickname,
			IsChatNode:       item.IsChatNode,
			IsRepeater:       item.IsRepeater,
			IsRoomServer:     item.IsRoomServer,
			IsSensor:         item.IsSensor,
			HasLocation:      item.HasLocation,
			Latitude:         item.Latitude,
			Longitude:        item.Longitude,
			SignatureValid:   item.SignatureValid,
			HopCount:         item.HopCount,
			HopStations:      item.HopStations,
			TimeDriftSeconds: item.TimeDriftSeconds,
			AdvertTimestamp:  item.AdvertTimestamp,
			FirstReceivedAt:  item.FirstReceivedAt,
			LastReceivedAt:   item.LastReceivedAt,
		})
	}
	return page, nil
}
