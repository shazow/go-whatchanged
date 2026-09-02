package vfs

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestOverlayReadDirPreservesDirectorySymlink(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "target"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	overlay := NewOverlay()
	entries, err := overlay.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "link" {
			continue
		}
		if entry.IsDir() {
			t.Error("directory symlink reported as a directory during enumeration")
		}
		if entry.Mode()&fs.ModeSymlink == 0 {
			t.Error("directory symlink mode was not preserved")
		}
		if !overlay.IsDir(filepath.Join(dir, "link")) {
			t.Error("an explicitly addressed directory symlink was not followed")
		}
		return
	}
	t.Fatal("directory symlink missing from listing")
}
