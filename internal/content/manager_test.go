package content

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestContentIdentitySurvivesRenameAndRestart(t *testing.T) {
	root := t.TempDir()
	manager, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	first, err := manager.Add("text/old", "")
	if err != nil {
		t.Fatal(err)
	}
	renamed, err := manager.Rename(first.ID, "text/new", first.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if renamed.ID != first.ID || renamed.Revision != 2 {
		t.Fatalf("unexpected rename: %#v", renamed)
	}
	reloaded, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reloaded.Resolve("text/new")
	if !ok || got.ID != first.ID || got.Revision != 2 {
		t.Fatalf("identity was not durable: %#v", got)
	}
	if _, ok := reloaded.Resolve("text/old"); ok {
		t.Fatal("old path still resolves")
	}
}

func TestContentMutationRejectsStaleRevision(t *testing.T) {
	manager, _ := NewManager(t.TempDir())
	record, _ := manager.Add("text/item", "")
	if _, err := manager.Edit(record.ID, record.Revision); err != nil {
		t.Fatal(err)
	}
	current, err := manager.Edit(record.ID, record.Revision)
	if !errors.Is(err, ErrRevisionConflict) || current.Revision != 2 {
		t.Fatalf("wanted revision conflict, got %#v %v", current, err)
	}
}

func TestContentAcceptsClientGeneratedUUID(t *testing.T) {
	manager, _ := NewManager(t.TempDir())
	want := "0195e6c7-1234-4123-8123-123456789abc"
	got, err := manager.Add("text/offline", want)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != want {
		t.Fatalf("wanted %s, got %s", want, got.ID)
	}
	if again, _ := manager.Add("text/offline", ""); again.ID != want {
		t.Fatal("client identity was not stable")
	}
}

func TestContentRenameAndDeleteMoveAllMetadata(t *testing.T) {
	root := t.TempDir()
	manager, _ := NewManager(root)
	record, _ := manager.Add("text/one", "")
	manager.now = func() time.Time { return time.Unix(200, 0) }
	record, err := manager.SetFavorite(record.ID, true, record.Revision)
	if err != nil {
		t.Fatal(err)
	}
	record, err = manager.Rename(record.ID, "text/two", record.Revision)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := manager.View("text/two", time.Time{})
	if !snapshot.Favorite {
		t.Fatal("favorite was not migrated")
	}
	if _, ok := manager.Resolve("text/one"); ok {
		t.Fatal("old storage remains")
	}
	if _, err = manager.Remove(record.ID, record.Revision); err != nil {
		t.Fatal(err)
	}
	reloaded, _ := NewManager(root)
	if _, ok := reloaded.Resolve(record.ID); ok {
		t.Fatal("identity was not removed")
	}
}

func TestContentMigratesLegacyMetadata(t *testing.T) {
	root := t.TempDir()
	id := "0195e6c7-1234-4123-8123-123456789abc"
	_ = os.WriteFile(filepath.Join(root, "identities.json"), []byte(`{"byId":{"`+id+`":{"id":"`+id+`","storageId":"text/one","revision":4}},"byStorage":{"text/one":"`+id+`"}}`), 0644)
	_ = os.WriteFile(filepath.Join(root, "favorites.json"), []byte(`{"text/one":true}`), 0644)
	manager, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := manager.View("text/one", time.Unix(100, 0))
	if snapshot.ID != id || snapshot.Revision != 4 || !snapshot.Favorite {
		t.Fatalf("legacy metadata lost: %#v", snapshot)
	}
}

func TestFailedMetadataCommitDoesNotChangeInMemoryState(t *testing.T) {
	root := t.TempDir()
	manager, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	record, err := manager.Add("text/original", "")
	if err != nil {
		t.Fatal(err)
	}
	manager.path = root
	if _, err = manager.SetFavorite(record.ID, true, record.Revision); err == nil {
		t.Fatal("favorite mutation unexpectedly committed")
	}
	current, ok := manager.Resolve(record.ID)
	if !ok || current.Revision != record.Revision || current.Storage != record.Storage {
		t.Fatalf("failed favorite changed identity: %#v", current)
	}
	if snapshot := manager.View(record.Storage, time.Time{}); snapshot.Favorite {
		t.Fatal("failed favorite remained in memory")
	}
	if _, err = manager.Rename(record.ID, "text/renamed", record.Revision); err == nil {
		t.Fatal("rename unexpectedly committed")
	}
	if _, ok = manager.Resolve("text/renamed"); ok {
		t.Fatal("failed rename remained in memory")
	}
	if current, ok = manager.Resolve("text/original"); !ok || current.Revision != record.Revision {
		t.Fatalf("original identity was not restored: %#v", current)
	}
	if _, err = manager.Remove(record.ID, record.Revision); err == nil {
		t.Fatal("remove unexpectedly committed")
	}
	if _, ok = manager.Resolve(record.ID); !ok {
		t.Fatal("failed remove remained in memory")
	}
}
