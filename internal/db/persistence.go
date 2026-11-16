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
// schemas, and starts the background advert and group-text writers. retention
// bounds the advert history (see AdvertRetention) and groupTexts the per-
// channel group-text history (see GroupTextsRetention); zero structs keep
// both for ever.
func NewPersistence(dbPath string, retention AdvertRetention, groupTexts GroupTextsRetention) (*Persistence, error) {
	db, err := OpenKamDB(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open persistence db: %w", err)
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
		AdvertQueue:    NewAdvertQueue(db, retention),
		GroupTextQueue: NewGroupTextQueue(db, groupTexts),
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
