package runtime

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
)

// WriteControlFile atomically replaces a wrapper control file. The resulting
// file is owner-readable/writable only; temp file, file contents, and parent
// directory are synced so a successful return survives a crash boundary.
func WriteControlFile(path string, contents []byte) error {
	dir := filepath.Dir(path)
	name := filepath.Base(path)
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return fmt.Errorf("runtime: random temporary file name: %w", err)
	}
	tmp := filepath.Join(dir, fmt.Sprintf(".%s.%x.tmp", name, random))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("runtime: create temporary control file: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(contents); err != nil {
		_ = f.Close()
		return fmt.Errorf("runtime: write temporary control file: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("runtime: sync temporary control file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("runtime: close temporary control file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("runtime: rename control file: %w", err)
	}
	cleanup = false
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("runtime: open control directory: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("runtime: sync control directory: %w", err)
	}
	return nil
}
