// Package atomicfile writes files so a crash or power cut can never leave a
// half-written or zero-filled file behind.
//
// Windows (NTFS) can keep a file's new size after a crash while the data
// never reached the disk, which reads back as NUL bytes. So every write here
// goes to a temp file, is fsynced, and only then replaces the target; the
// previous good copy is kept as <name>.bak and Read falls back to it.
package atomicfile

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
)

// Write replaces path with data durably, keeping the old file as path.bak.
func Write(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := writeSynced(tmp, data, perm); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		// Keep the last good copy. If we crash between these two renames,
		// Read finds no file and uses the .bak.
		if err := os.Rename(path, path+".bak"); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

// WriteInPlace rewrites path without replacing the file object (so its
// Windows ACL stays), after first saving a synced copy at path.new that a
// crash mid-write can be recovered from. Use for small, rarely written
// files whose permissions matter (wallets.json).
func WriteInPlace(path string, data []byte, perm os.FileMode) error {
	side := path + ".new"
	if err := writeSynced(side, data, perm); err != nil {
		return err
	}
	if err := writeSynced(path, data, perm); err != nil {
		return err
	}
	return os.Remove(side)
}

func writeSynced(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync() // not supported on every OS; best effort
		d.Close()
	}
}

// ErrCorrupt means the file exists but holds NUL bytes or is empty.
var ErrCorrupt = errors.New("file is empty or zero-filled (unclean shutdown)")

// Read returns path's content. When the file is missing or zero-filled it
// falls back to path.bak and reports which one it used.
func Read(path string) (data []byte, from string, err error) {
	data, err = os.ReadFile(path)
	if err == nil && looksWritten(data) {
		return data, path, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, "", err
	}
	bak, berr := os.ReadFile(path + ".bak")
	if berr == nil && looksWritten(bak) {
		return bak, path + ".bak", nil
	}
	if err != nil {
		return nil, "", err // plain "does not exist": a first run
	}
	return nil, "", ErrCorrupt
}

func looksWritten(b []byte) bool {
	t := bytes.TrimSpace(b)
	return len(t) > 0 && t[0] != 0
}
