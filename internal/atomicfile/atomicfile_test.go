package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteKeepsBackupAndReadFallsBack(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	if err := Write(p, []byte(`{"v":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Write(p, []byte(`{"v":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p + ".bak"); string(b) != `{"v":1}` {
		t.Fatalf("backup %q", b)
	}
	// Simulate an unclean shutdown: the file keeps its size, data is zeros.
	os.WriteFile(p, make([]byte, 64), 0o600)
	b, from, err := Read(p)
	if err != nil || string(b) != `{"v":1}` || from != p+".bak" {
		t.Fatalf("read after zero-fill: %q from %s err %v", b, from, err)
	}
	// Crash between the two renames: no file, the .bak is used.
	os.Remove(p)
	if b, from, _ = Read(p); string(b) != `{"v":1}` || from != p+".bak" {
		t.Fatalf("read after missing file: %q from %s", b, from)
	}
	// Both gone or both zero: missing is a first run, zero-filled is corrupt.
	os.Remove(p + ".bak")
	if _, _, err = Read(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
	os.WriteFile(p, make([]byte, 8), 0o600)
	if _, _, err = Read(p); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt: %v", err)
	}
}

func TestWriteInPlaceLeavesNoSideFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "wallets.json")
	os.WriteFile(p, []byte("old"), 0o600)
	if err := WriteInPlace(p, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "new" {
		t.Fatalf("content %q", b)
	}
	if _, err := os.Stat(p + ".new"); !os.IsNotExist(err) {
		t.Fatal("side copy left behind")
	}
}
