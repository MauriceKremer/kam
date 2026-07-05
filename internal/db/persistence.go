package db

import (
	"database/sql"
	"fmt"

	"kam.bytecoder.nl/internal/kiss/parsers"
)

// Persistence combines advert and group-text queues over a single SQLite
// handle. It satisfies the kiss.Persistence interface used by Manager.
type Persistence struct {
	db *sql.DB

	AdvertQueue    *AdvertQueue
	GroupTextQueue *GroupTextQueue
}

// NewPersistence opens the SQLite database at dbPath, ensures all required
// schemas, and starts the background advert and group-text writers.
func NewPersistence(dbPath string) (*Persistence, error) {
	db, err := OpenKamDB(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open persistence db: %w", err)
	}

	if err := EnsureChannelsSchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("ensure channels schema: %w", err)
	}
	if err := EnsureAdvertsSchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("ensure adverts schema: %w", err)
	}
	if err := EnsureGroupTextsSchema(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("ensure group texts schema: %w", err)
	}

	p := &Persistence{
		db:             db,
		AdvertQueue:    NewAdvertQueue(db),
		GroupTextQueue: NewGroupTextQueue(db),
	}
	return p, nil
}

// EnqueueAdvert forwards the advert to the advert queue.
func (p *Persistence) EnqueueAdvert(advert parsers.AdvertPacket) bool {
	return p.AdvertQueue.Enqueue(advert)
}

// EnqueueGroupText forwards the message to the group-text queue.
func (p *Persistence) EnqueueGroupText(msg parsers.GroupTextMessage) bool {
	return p.GroupTextQueue.Enqueue(msg)
}

// Close stops both writers and closes the shared database handle.
func (p *Persistence) Close() error {
	p.AdvertQueue.Stop()
	p.GroupTextQueue.Stop()

	if err := p.db.Close(); err != nil {
		return fmt.Errorf("close persistence db: %w", err)
	}
	return nil
}
