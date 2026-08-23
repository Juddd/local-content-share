package main

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestIdentitySurvivesRenameAndRestart(t *testing.T) {
	name := filepath.Join(t.TempDir(), "identities.json")
	s := newIdentityStore(name)
	first := s.Ensure("text/old")
	renamed, err := s.Mutate(first.ID, "text/new", first.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if renamed.ID != first.ID || renamed.Revision != 2 {
		t.Fatalf("unexpected rename: %#v", renamed)
	}
	reloaded := newIdentityStore(name)
	got, ok := reloaded.Resolve("text/new")
	if !ok || got.ID != first.ID || got.Revision != 2 {
		t.Fatalf("identity was not durable: %#v", got)
	}
	if _, ok := reloaded.Resolve("text/old"); ok {
		t.Fatal("old path still resolves")
	}
}

func TestIdentityRejectsStaleRevision(t *testing.T) {
	s := newIdentityStore(filepath.Join(t.TempDir(), "identities.json"))
	r := s.Ensure("text/item")
	if _, err := s.Mutate(r.ID, "", r.Revision); err != nil {
		t.Fatal(err)
	}
	current, err := s.Mutate(r.ID, "", r.Revision)
	if !errors.Is(err, errRevisionConflict) || current.Revision != 2 {
		t.Fatalf("wanted revision conflict, got %#v %v", current, err)
	}
}

func TestIdentityAcceptsClientGeneratedUUID(t *testing.T) {
	s := newIdentityStore(filepath.Join(t.TempDir(), "identities.json"))
	want := "0195e6c7-1234-4123-8123-123456789abc"
	got := s.EnsureWithID("text/offline", want)
	if got.ID != want {
		t.Fatalf("wanted client identity %s, got %s", want, got.ID)
	}
	if again := s.Ensure("text/offline"); again.ID != want {
		t.Fatal("client identity was not stable")
	}
}

func TestIdentityMigratesLegacyLinkStorageWithoutChangingID(t *testing.T) {
	s := newIdentityStore(filepath.Join(t.TempDir(), "identities.json"))
	old := s.Ensure("link/Title%09https:%2F%2Fexample.com")
	s.MigrateStorage("link/Title%09https:%2F%2Fexample.com", "link/Title\thttps://example.com")
	got, ok := s.Resolve("link/Title\thttps://example.com")
	if !ok || got.ID != old.ID {
		t.Fatalf("link identity changed: %#v", got)
	}
	if _, ok := s.Resolve("link/Title%09https:%2F%2Fexample.com"); ok {
		t.Fatal("legacy link storage remains")
	}
}
