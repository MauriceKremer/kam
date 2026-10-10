package db

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestMigrateRealKamDbOnce copies the repo-root kam.db (if present) to a temp
// file, runs EnsureAdvertsSchema, and adds the automation schema, proving the
// startup path the app uses succeeds against the actual legacy database.
// Skipped in CI / when kam.db is absent. Does not mutate the real file.
func TestMigrateRealKamDbOnce(t *testing.T) {
	src := filepath.Join("..", "..", "kam.db")
	if _, err := os.Stat(src); err != nil {
		t.Skip("kam.db not present at repo root")
	}
	dst := filepath.Join(t.TempDir(), "kam.db")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read kam.db: %v", err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatalf("copy kam.db: %v", err)
	}

	db, err := OpenKamDB(dst)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := EnsureAdvertsSchema(db); err != nil {
		t.Fatalf("ensure adverts schema on real kam.db copy: %v", err)
	}
	if err := EnsureAdvertsSchema(db); err != nil {
		t.Fatalf("second ensure (idempotency) on real kam.db copy: %v", err)
	}

	cols, err := tableColumns(db, "adverts")
	if err != nil {
		t.Fatalf("table columns: %v", err)
	}
	if !cols["hop_count"] || !cols["hop_stations"] {
		t.Fatalf("hop columns missing after migration: %v", cols)
	}

	repository, err := OpenAutomationRepository(dst)
	if err != nil {
		t.Fatalf("open automation repository on real kam.db copy: %v", err)
	}
	defer repository.Close()
	if _, err := repository.Save(context.Background(), AutomationScript{ID: "probe", Name: "probe", Source: "def on_group_text(message):\n    pass\n", TimeoutMS: 500}, 0); err != nil {
		t.Fatalf("save script on real kam.db copy: %v", err)
	}
	scripts, err := repository.List(context.Background())
	if err != nil || len(scripts) == 0 {
		t.Fatalf("automation scripts missing after save on real kam.db copy: %+v, error %v", scripts, err)
	}
}
