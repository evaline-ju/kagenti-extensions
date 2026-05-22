package spiffe

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicWrite_NewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "svid.pem")
	if err := atomicWrite(path, []byte("contents"), 0o644); err != nil {
		t.Fatalf("atomicWrite: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "contents" {
		t.Errorf("got %q, want %q", got, "contents")
	}
}

func TestAtomicWrite_OverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "svid.pem")
	_ = os.WriteFile(path, []byte("old"), 0o644)
	if err := atomicWrite(path, []byte("new"), 0o644); err != nil {
		t.Fatalf("atomicWrite: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "new" {
		t.Errorf("got %q, want %q", got, "new")
	}
}

func TestAtomicWrite_NoTempLeftOnSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "svid.pem")
	if err := atomicWrite(path, []byte("contents"), 0o644); err != nil {
		t.Fatalf("atomicWrite: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("expected 1 entry, got %d: %v", len(entries), entries)
	}
}
