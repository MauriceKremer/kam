package node

import (
	"context"

	"kam.bytecoder.nl/internal/api"
	"kam.bytecoder.nl/internal/db"
	"kam.bytecoder.nl/internal/models"
)

type apiChannels struct {
	persistence *db.Persistence
	configured  []models.ChannelConfig
}

func (r apiChannels) Channels(ctx context.Context) ([]api.Channel, error) {
	stored, err := r.persistence.ListMonitorChannels(ctx, r.configured)
	if err != nil {
		return nil, err
	}
	channels := make([]api.Channel, 0, len(stored))
	for _, item := range stored {
		channels = append(channels, api.Channel{Name: item.Name, KeyStrategy: item.KeyStrategy, Enabled: item.Enabled, StoredMessages: item.StoredMessages, LastMessageAt: item.LastMessageAt})
	}
	return channels, nil
}

func (r apiChannels) Messages(ctx context.Context, name string, limit int, beforeID int64, query string) (api.ChannelPage, error) {
	stored, err := r.persistence.ListChannelMessages(ctx, name, limit, beforeID, query)
	if err != nil {
		return api.ChannelPage{}, err
	}
	page := api.ChannelPage{Messages: make([]api.ChannelMessage, 0, len(stored.Messages)), NextBeforeID: stored.NextBeforeID}
	for _, item := range stored.Messages {
		page.Messages = append(page.Messages, api.ChannelMessage{ID: item.ID, ReceivedAt: item.ReceivedAt, Sender: item.Sender, Body: item.Body, HopCount: item.HopCount, SNRDB: item.SNRDB, RSSIDBm: item.RSSIDBm})
	}
	return page, nil
}
