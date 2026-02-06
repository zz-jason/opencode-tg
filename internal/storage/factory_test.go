package storage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewStore_File(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "test.json")
	opts := Options{
		Type:     "file",
		FilePath: path,
	}
	store, err := NewStore(opts)
	if err != nil {
		t.Fatalf("NewStore failed for file type: %v", err)
	}
	if store == nil {
		t.Fatal("NewStore should return non-nil store for file type")
	}
	defer store.Close()

	// Store something to ensure file is created
	err = store.StoreUserSession(1, "test-session")
	if err != nil {
		t.Fatalf("StoreUserSession failed: %v", err)
	}

	// Verify file created
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Errorf("Storage file should be created at %s", path)
	}
}

func TestNewStore_DefaultType(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "test.json")
	opts := Options{
		Type:     "", // empty type should default to file
		FilePath: path,
	}
	store, err := NewStore(opts)
	if err != nil {
		t.Fatalf("NewStore failed for empty type: %v", err)
	}
	if store == nil {
		t.Fatal("NewStore should return non-nil store for empty type")
	}
	defer store.Close()
}

func TestNewStore_FileMissingPath(t *testing.T) {
	opts := Options{
		Type: "file",
		// FilePath empty
	}
	store, err := NewStore(opts)
	if err == nil {
		t.Error("NewStore should return error for file type without path")
	}
	if store != nil {
		t.Error("NewStore should return nil store on error")
		store.Close()
	}
}

func TestNewStore_InvalidType(t *testing.T) {
	opts := Options{
		Type: "invalid",
	}
	store, err := NewStore(opts)
	if err == nil {
		t.Error("NewStore should return error for invalid type")
	}
	if store != nil {
		t.Error("NewStore should return nil store on error")
		store.Close()
	}
}
