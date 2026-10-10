package mesh

import (
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// LoadOrCreateIdentity reads the identity seed stored at path, or generates and
// stores a new one when the file does not exist. The file holds the 32-byte
// seed as one line of hex and is written with mode 0600 in a 0700 directory.
// reject can veto a freshly generated hash (see GenerateIdentity); it does not
// apply to a key that is already stored. created reports whether a new key was
// written.
func LoadOrCreateIdentity(path string, random io.Reader, reject func(hash byte) bool) (id *Identity, created bool, err error) {
	data, err := os.ReadFile(path)
	if err == nil {
		id, err := parseKeyFile(data)
		if err != nil {
			return nil, false, fmt.Errorf("identity file %q: %w", path, err)
		}
		return id, false, nil
	}
	if !os.IsNotExist(err) {
		return nil, false, fmt.Errorf("read identity file %q: %w", path, err)
	}

	id, err = GenerateIdentity(random, reject)
	if err != nil {
		return nil, false, err
	}
	if err := writeKeyFile(path, id.Seed()); err != nil {
		return nil, false, err
	}
	return id, true, nil
}

func parseKeyFile(data []byte) (*Identity, error) {
	seed, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, fmt.Errorf("seed is not valid hex: %w", err)
	}
	id, err := NewIdentity(seed)
	if err != nil {
		return nil, err
	}
	if !ValidHash(id.Hash()) {
		return nil, fmt.Errorf("public key hash %#02x is reserved", id.Hash())
	}
	return id, nil
}

// writeKeyFile stores the seed atomically: a half-written key file would
// otherwise change the room's address on the next start.
func writeKeyFile(path string, seed []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create identity directory %q: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".identity.*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary identity file: %w", err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("restrict identity file: %w", err)
	}
	if _, err := tmp.WriteString(hex.EncodeToString(seed) + "\n"); err != nil {
		return fmt.Errorf("write identity file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync identity file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close identity file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("install identity file %q: %w", path, err)
	}
	committed = true
	return nil
}
