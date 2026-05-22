package spiffe

import (
	"fmt"
	"os"
	"path/filepath"
)

// atomicWrite writes data to path via a tmp file in the same directory
// followed by os.Rename. Same-directory rename is atomic on POSIX, which
// guarantees external readers always see either the old or the new file
// content — never a partial write.
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("atomicWrite: create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("atomicWrite: write %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("atomicWrite: chmod %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("atomicWrite: close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("atomicWrite: rename %s -> %s: %w", tmpName, path, err)
	}
	return nil
}
