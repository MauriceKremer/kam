package room

import (
	"fmt"
	"io"
	"path/filepath"

	"kam.bytecoder.nl/internal/mesh"
	"kam.bytecoder.nl/internal/models"
)

// KeyDir returns the directory holding room identity files: a "rooms"
// directory next to the database.
func KeyDir(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), "rooms")
}

// IdentityPath returns the identity file of a room.
func IdentityPath(keyDir, roomID string) string {
	return filepath.Join(keyDir, roomID+".key")
}

// LoadedIdentity is a room identity and whether it was just generated.
type LoadedIdentity struct {
	Identity *mesh.Identity
	Created  bool
}

// LoadIdentities loads (or, on first use, generates and stores) one identity
// per room, in order. A newly generated key never reuses the first byte of an
// earlier room's key, so inbound packets address exactly one room
// (RS-MULTI-3). Keys that already exist are used as stored.
func LoadIdentities(keyDir string, rooms []models.RoomConfig, random io.Reader) ([]LoadedIdentity, error) {
	taken := map[byte]bool{}
	out := make([]LoadedIdentity, 0, len(rooms))
	for _, room := range rooms {
		id, created, err := mesh.LoadOrCreateIdentity(IdentityPath(keyDir, room.ID), random, func(hash byte) bool { return taken[hash] })
		if err != nil {
			return nil, fmt.Errorf("room %q: %w", room.ID, err)
		}
		taken[id.Hash()] = true
		out = append(out, LoadedIdentity{Identity: id, Created: created})
	}
	return out, nil
}
