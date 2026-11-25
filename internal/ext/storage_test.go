package ext

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestStorageCRUDQuotaAndPrefix(t *testing.T) {
	dataDir := t.TempDir()
	path := StoragePath(dataDir, "demo-ext")
	st := NewStorage(path, 0)
	if st.Quota() != DefaultStorageQuota {
		t.Fatalf("Quota() = %d, want %d", st.Quota(), DefaultStorageQuota)
	}

	// Empty key, >128B key, invalid UTF-8 key/value rejected.
	if _, err := st.Set("", "v"); err == nil {
		t.Fatal("expected error for empty key")
	}
	if _, err := st.Set(strings.Repeat("k", MaxStorageKeyBytes+1), "v"); err == nil {
		t.Fatal("expected error for 129B key")
	}
	if _, err := st.Set(string([]byte{0xff}), "v"); err == nil {
		t.Fatal("expected error for invalid UTF-8 key")
	}
	if _, err := st.Set("k", string([]byte{0xff})); err == nil {
		t.Fatal("expected error for invalid UTF-8 value")
	}
	for _, badKey := range []string{"bad\x00key", "bad\nkey", "bad\u202ekey"} {
		if _, err := st.Set(badKey, "v"); err == nil {
			t.Fatalf("expected error for control/spoof key %q", badKey)
		}
		if _, _, err := st.Get(badKey); err == nil {
			t.Fatalf("expected Get error for control/spoof key %q", badKey)
		}
		if _, err := st.Delete(badKey); err == nil {
			t.Fatalf("expected Delete error for control/spoof key %q", badKey)
		}
	}

	// 128B key accepted.
	key128 := strings.Repeat("a", MaxStorageKeyBytes)
	used, err := st.Set(key128, "hello")
	if err != nil {
		t.Fatalf("Set(128B key): %v", err)
	}
	if used != MaxStorageKeyBytes+5 {
		t.Fatalf("used = %d, want %d", used, MaxStorageKeyBytes+5)
	}

	// Prefix listing sorted.
	for _, k := range []string{"job/2", "job/10", "job/1", "other"} {
		if _, err := st.Set(k, "x"); err != nil {
			t.Fatal(err)
		}
	}
	keys, err := st.Keys("job/")
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	want := []string{"job/1", "job/10", "job/2"}
	if !slices.Equal(keys, want) {
		t.Fatalf("Keys(job/) = %v, want %v", keys, want)
	}

	// Reload from disk.
	st.Reset()
	val, ok, err := st.Get(key128)
	if err != nil || !ok || val != "hello" {
		t.Fatalf("Get after Reset = (%q, %v, %v), want (hello, true, nil)", val, ok, err)
	}

	// Delete.
	deleted, err := st.Delete("other")
	if err != nil || !deleted {
		t.Fatalf("Delete(other) = (%v, %v), want (true, nil)", deleted, err)
	}
	if deleted2, err := st.Delete("other"); err != nil || deleted2 {
		t.Fatalf("Delete(missing) = (%v, %v), want (false, nil)", deleted2, err)
	}
}

func TestStorageQuotaEnforcementAndRollback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "storage.json")
	st := NewStorage(path, 32)

	if _, err := st.Set("k1", strings.Repeat("a", 20)); err != nil { // 2 + 20 = 22
		t.Fatalf("Set k1: %v", err)
	}
	// Overwrite-grow exceeding 32 bytes must fail and preserve old value.
	if _, err := st.Set("k1", strings.Repeat("b", 31)); err == nil {
		t.Fatal("expected quota error on overwrite-grow")
	}
	if v, ok, _ := st.Get("k1"); !ok || v != strings.Repeat("a", 20) {
		t.Fatalf("k1 after failed grow = (%q, %v), want original", v, ok)
	}

	// Overwrite-shrink frees space.
	if used, err := st.Set("k1", "a"); err != nil || used != 3 {
		t.Fatalf("Set shrink = (%d, %v), want (3, nil)", used, err)
	}

	// Null JSON file must be refused.
	nullPath := filepath.Join(dir, "null.json")
	if err := os.WriteFile(nullPath, []byte("null"), 0o600); err != nil {
		t.Fatal(err)
	}
	nullSt := NewStorage(nullPath, 0)
	if _, err := nullSt.Keys(""); err == nil {
		t.Fatal("expected error loading null JSON storage")
	}

	// Save failure rolls back in-memory state: point path at an existing directory.
	badPath := filepath.Join(dir, "is-a-dir")
	if err := os.MkdirAll(badPath, 0o700); err != nil {
		t.Fatal(err)
	}
	badSt := NewStorage(badPath, 100)
	if _, err := badSt.Set("x", "1"); err == nil {
		t.Fatal("expected save failure when path is a directory")
	}
	if _, ok, _ := badSt.Get("x"); ok {
		t.Fatal("in-memory state was not rolled back on save failure")
	}
}
