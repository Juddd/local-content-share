package main

import (
	"path/filepath"
	"testing"
)

func TestFavoriteStorePersistsRenameAndDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "favorites.json")
	store := newFavoriteStore(path)
	if err := store.Set("text/one", true); err != nil {
		t.Fatal(err)
	}
	if !newFavoriteStore(path).Is("text/one") {
		t.Fatal("favorite was not persisted")
	}
	if err := store.Rename("text/one", "text/two"); err != nil {
		t.Fatal(err)
	}
	if store.Is("text/one") || !store.Is("text/two") {
		t.Fatal("favorite was not migrated on rename")
	}
	if err := store.Delete("text/two"); err != nil {
		t.Fatal(err)
	}
	if newFavoriteStore(path).Is("text/two") {
		t.Fatal("favorite was not removed")
	}
}
