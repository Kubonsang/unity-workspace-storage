//go:build windows

package storage

import (
	"os"
	"path/filepath"
	"testing"
)

// Exercises the real CreateVirtualDisk/OpenVirtualDisk APIs without attaching
// volumes. Existing 2 MiB parents and children must remain readable unchanged.
func TestDifferencingChildGeometry(t *testing.T) {
	if os.Getenv("UNITY_WORKSPACE_STORAGE_GEOMETRY_TEST") != "1" {
		t.Skip("set UNITY_WORKSPACE_STORAGE_GEOMETRY_TEST=1 on Windows NTFS")
	}
	root := t.TempDir()
	parent := filepath.Join(root, "parent.vhdx")
	old := filepath.Join(root, "retained.vhdx")
	fresh := filepath.Join(root, "new.vhdx")
	if err := CreateDynamicWithOptions(parent, CreateOptions{MaximumSize: 64 << 20, BlockSizeInBytes: 2 << 20, SectorSizeInBytes: 4096}); err != nil {
		t.Fatal(err)
	}
	if err := createVHDX(old, CreateOptions{BlockSizeInBytes: 2 << 20}, parent); err != nil {
		t.Fatal(err)
	}
	if err := CreateDifferencing(fresh, parent); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		path  string
		block uint32
		child bool
	}{{parent, 2 << 20, false}, {old, 2 << 20, true}, {fresh, 1 << 20, true}} {
		a, err := Open(item.path, true)
		if err != nil {
			t.Fatal(err)
		}
		size, err := a.Size()
		if err == nil && item.child {
			err = a.VerifyParent(parent)
		}
		closeErr := a.CloseHandle()
		if err != nil {
			t.Fatal(err)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
		if size.BlockSize != item.block || size.VirtualSize != 64<<20 || size.SectorSize != 4096 {
			t.Fatalf("%s geometry=%+v", item.path, size)
		}
	}
}
