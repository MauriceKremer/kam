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
	persistence, err := NewPersistence(path, AdvertRetention{}, GroupTextsRetention{})
	if err != nil {
		t.Fatal(err)
	}
	if err := persistence.Close(); err != nil {
		t.Fatal(err)
	}
	repository := openAutomationTestRepository(t, path)
	saveScript(t, repository, "first", "first")
	// adverts created by NewPersistence must survive automation schema creation.
	if _, err := repository.db.Exec(`SELECT 1 FROM adverts LIMIT 1`); err != nil {
		t.Fatalf("legacy adverts table damaged: %v", err)
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

func TestAutomationRepositoryJournalsReplacedVersions(t *testing.T) {
	repository := openAutomationTestRepository(t, filepath.Join(t.TempDir(), "kam.db"))
	ctx := context.Background()
	first := saveScript(t, repository, "journal", "journal")
	if versions, err := repository.Versions(ctx, first.ID); err != nil || len(versions) != 0 {
		t.Fatalf("new script must have no journal: %+v, error %v", versions, err)
	}

	second, err := repository.Save(ctx, AutomationScript{ID: first.ID, Name: first.Name, Source: first.Source + "# v2\n", Enabled: true, TimeoutMS: 250}, first.Revision)
	if err != nil {
		t.Fatal(err)
	}
	versions, err := repository.Versions(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 {
		t.Fatalf("update must journal one version: %+v", versions)
	}
	old := versions[0]
	if old.Revision != first.Revision || old.Name != first.Name || old.Source != first.Source || old.TimeoutMS != first.TimeoutMS || old.Enabled {
		t.Fatalf("journaled version does not match the replaced row: %+v", old)
	}
	if old.ArchivedAt == 0 {
		t.Fatal("journaled version has no archived_at")
	}
	if old.Fingerprint != sourceFingerprint(old.Source) || len(old.Fingerprint) != 16 {
		t.Fatalf("journaled version fingerprint: %q", old.Fingerprint)
	}
	// The fingerprint is stored on the row, not derived at read time.
	var storedFingerprint string
	if err := repository.db.QueryRow(`SELECT fingerprint FROM automation_script_journal WHERE script_id = ? AND revision = ?`, first.ID, first.Revision).Scan(&storedFingerprint); err != nil || storedFingerprint != old.Fingerprint {
		t.Fatalf("journal row fingerprint: %q, error %v", storedFingerprint, err)
	}

	// A stale update changes nothing and journals nothing.
	if _, err := repository.Save(ctx, AutomationScript{ID: first.ID, Name: first.Name, Source: "x", TimeoutMS: 500}, first.Revision); !errors.Is(err, ErrAutomationConflict) {
		t.Fatalf("stale update: got %v", err)
	}
	if versions, _ := repository.Versions(ctx, first.ID); len(versions) != 1 {
		t.Fatalf("failed update journaled a version: %+v", versions)
	}

	// Each successful change appends one revision; newest is listed first.
	if _, err := repository.Save(ctx, AutomationScript{ID: second.ID, Name: second.Name, Source: second.Source, Enabled: false, TimeoutMS: second.TimeoutMS}, second.Revision); err != nil {
		t.Fatal(err)
	}
	versions, err = repository.Versions(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 2 || versions[0].Revision != second.Revision || versions[1].Revision != first.Revision {
		t.Fatalf("versions not in newest-first order: %+v", versions)
	}
}

func TestAutomationRepositoryJournalsDeletedVersion(t *testing.T) {
	repository := openAutomationTestRepository(t, filepath.Join(t.TempDir(), "kam.db"))
	ctx := context.Background()
	script := saveScript(t, repository, "gone", "gone")

	if err := repository.Delete(ctx, script.ID, script.Revision); err != nil {
		t.Fatal(err)
	}
	versions, err := repository.Versions(ctx, script.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 || versions[0].Revision != script.Revision || versions[0].Source != script.Source {
		t.Fatalf("delete must journal the final version: %+v", versions)
	}

	// Repeating the delete of a now-missing script journals nothing new.
	if err := repository.Delete(ctx, script.ID, script.Revision); !errors.Is(err, ErrAutomationNotFound) {
		t.Fatalf("second delete: got %v", err)
	}
	if versions, _ := repository.Versions(ctx, script.ID); len(versions) != 1 {
		t.Fatalf("failed delete journaled a version: %+v", versions)
	}
}
