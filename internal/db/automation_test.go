package db

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

func openAutomationTestRepository(t *testing.T, path string) *AutomationRepository {
	t.Helper()
	repository, err := OpenAutomationRepository(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	return repository
}

func saveScript(t *testing.T, repository *AutomationRepository, id, name string) AutomationScript {
	t.Helper()
	saved, err := repository.Save(context.Background(), AutomationScript{ID: id, Name: name, Source: "def on_group_text(message):\n    pass\n", TimeoutMS: 500}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

func TestAutomationSchemaCoexistsWithLegacyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kam.db")
	persistence, err := NewPersistence(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := persistence.Close(); err != nil {
		t.Fatal(err)
	}
	repository := openAutomationTestRepository(t, path)
	saveScript(t, repository, "first", "first")
	columns, err := tableColumns(repository.db, "adverts")
	if err != nil || !columns["hop_count"] {
		t.Fatalf("legacy adverts table damaged: %v, error %v", columns, err)
	}
	if err := EnsureAutomationScriptsSchema(repository.db); err != nil {
		t.Fatalf("schema creation is not idempotent: %v", err)
	}
	if scripts, err := repository.List(context.Background()); err != nil || len(scripts) != 1 {
		t.Fatalf("scripts after reopening the schema: %+v, error %v", scripts, err)
	}
}

func TestAutomationRepositoryConcurrentWritersDoNotFail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kam.db")
	const writers = 8
	repositories := make([]*AutomationRepository, writers)
	for index := range repositories {
		repositories[index] = openAutomationTestRepository(t, path)
	}
	errs := make([]error, writers)
	var done sync.WaitGroup
	start := make(chan struct{})
	done.Add(writers)
	for index, repository := range repositories {
		go func() {
			defer done.Done()
			<-start
			id := "script-" + strconv.Itoa(index)
			_, errs[index] = repository.Save(context.Background(), AutomationScript{ID: id, Name: id, Source: "x", TimeoutMS: 500}, 0)
		}()
	}
	close(start)
	done.Wait()
	for index, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", index, err)
		}
	}
	if scripts, err := repositories[0].List(context.Background()); err != nil || len(scripts) != writers {
		t.Fatalf("scripts after concurrent writes: got %d, error %v", len(scripts), err)
	}
}

func TestAutomationRepositoryPersistsStatusAndLastError(t *testing.T) {
	repository := openAutomationTestRepository(t, filepath.Join(t.TempDir(), "kam.db"))
	ctx := context.Background()
	script := saveScript(t, repository, "status", "status")

	if err := repository.SetStatus(ctx, script.ID, "FROZEN", "timed out"); err != nil {
		t.Fatal(err)
	}
	frozen, err := repository.Get(ctx, script.ID)
	if err != nil || frozen.RuntimeStatus != "FROZEN" || frozen.LastError != "timed out" {
		t.Fatalf("frozen state not persisted: %+v, error %v", frozen, err)
	}
	if frozen.Revision != script.Revision {
		t.Fatalf("status change bumped revision: got %d, want %d", frozen.Revision, script.Revision)
	}

	if err := repository.SetStatus(ctx, script.ID, "BROKEN", "syntax error"); err != nil {
		t.Fatal(err)
	}
	if broken, _ := repository.Get(ctx, script.ID); broken.RuntimeStatus != "BROKEN" || broken.LastError != "syntax error" {
		t.Fatalf("broken state not persisted: %+v", broken)
	}

	if err := repository.SetStatus(ctx, script.ID, "READY", "ignored"); err != nil {
		t.Fatal(err)
	}
	if ready, _ := repository.Get(ctx, script.ID); ready.RuntimeStatus != "READY" || ready.LastError != "" {
		t.Fatalf("READY must clear last_error: %+v", ready)
	}

	withError, err := repository.Save(ctx, AutomationScript{ID: script.ID, Name: "status", Source: script.Source, TimeoutMS: 500, RuntimeStatus: "FROZEN", LastError: "kept"}, script.Revision)
	if err != nil || withError.LastError != "kept" {
		t.Fatalf("Save did not persist last_error: %+v, error %v", withError, err)
	}
	cleared, err := repository.Save(ctx, AutomationScript{ID: script.ID, Name: "status", Source: script.Source, TimeoutMS: 500, RuntimeStatus: "READY", LastError: "stale"}, withError.Revision)
	if err != nil || cleared.LastError != "" {
		t.Fatalf("Save to READY must clear last_error: %+v, error %v", cleared, err)
	}
	if err := repository.SetStatus(ctx, "missing", "FROZEN", "x"); !errors.Is(err, ErrAutomationNotFound) {
		t.Fatalf("SetStatus on missing script: got %v", err)
	}
	if err := repository.SetStatus(ctx, script.ID, "WEIRD", ""); err == nil {
		t.Fatal("invalid status accepted")
	}
}

func TestAutomationRepositorySaveRules(t *testing.T) {
	repository := openAutomationTestRepository(t, filepath.Join(t.TempDir(), "kam.db"))
	ctx := context.Background()
	first := saveScript(t, repository, "first", "first")
	saveScript(t, repository, "second", "second")

	if _, err := repository.Save(ctx, AutomationScript{ID: "bad", Name: "bad", Source: "x", TimeoutMS: 0}, 0); err == nil {
		t.Fatal("non-positive timeout accepted")
	}
	if _, err := repository.Save(ctx, AutomationScript{ID: " ", Name: "bad", Source: "x", TimeoutMS: 1}, 0); err == nil {
		t.Fatal("blank id accepted")
	}
	if _, err := repository.Save(ctx, AutomationScript{ID: "bad", Name: "bad", Source: "x", TimeoutMS: 1, RuntimeStatus: "WEIRD"}, 0); err == nil {
		t.Fatal("invalid runtime status accepted")
	}
	if _, err := repository.Save(ctx, AutomationScript{ID: "third", Name: "first", Source: "x", TimeoutMS: 1}, 0); !errors.Is(err, ErrAutomationNameTaken) {
		t.Fatalf("duplicate name on create: got %v", err)
	}
	if _, err := repository.Save(ctx, AutomationScript{ID: "second", Name: "first", Source: "x", TimeoutMS: 1}, 1); !errors.Is(err, ErrAutomationNameTaken) {
		t.Fatalf("duplicate name on update: got %v", err)
	}
	if _, err := repository.Save(ctx, AutomationScript{ID: "first", Name: "other", Source: "x", TimeoutMS: 1}, 0); !errors.Is(err, ErrAutomationConflict) {
		t.Fatalf("create over existing id: got %v", err)
	}
	if _, err := repository.Save(ctx, AutomationScript{ID: "first", Name: "first", Source: "x", TimeoutMS: 1}, 7); !errors.Is(err, ErrAutomationConflict) {
		t.Fatalf("stale revision: got %v", err)
	}
	if _, err := repository.Save(ctx, AutomationScript{ID: "ghost", Name: "ghost", Source: "x", TimeoutMS: 1}, 1); !errors.Is(err, ErrAutomationNotFound) {
		t.Fatalf("update of missing script: got %v", err)
	}
	updated, err := repository.Save(ctx, AutomationScript{ID: "first", Name: "first", Source: "changed", Enabled: true, TimeoutMS: 250}, first.Revision)
	if err != nil || updated.Revision != first.Revision+1 || updated.Source != "changed" || !updated.Enabled || updated.TimeoutMS != 250 {
		t.Fatalf("update did not apply: %+v, error %v", updated, err)
	}
	// Failed writes must leave the stored script untouched.
	if unchanged, _ := repository.Get(ctx, "first"); unchanged.Revision != updated.Revision || unchanged.Source != "changed" {
		t.Fatalf("failed write changed the script: %+v", unchanged)
	}
	if err := repository.Delete(ctx, "ghost", 1); !errors.Is(err, ErrAutomationNotFound) {
		t.Fatalf("delete missing: got %v", err)
	}
	if err := repository.Delete(ctx, "first", first.Revision); !errors.Is(err, ErrAutomationConflict) {
		t.Fatalf("delete stale revision: got %v", err)
	}
	if err := repository.Delete(ctx, "first", updated.Revision); err != nil {
		t.Fatal(err)
	}
}
