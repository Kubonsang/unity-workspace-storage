//go:build darwin || linux

package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestUnixLeaseSnapshotRecoversExactOwnedLease(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "parent")
	children := filepath.Join(root, "children")
	child := filepath.Join(children, "lease-recovery")
	mount := filepath.Join(root, "Library")
	if err := os.MkdirAll(parent, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(children, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "value"), []byte("parent"), 0600); err != nil {
		t.Fatal(err)
	}
	lease, _, err := NewBackend().Acquire(context.Background(), AcquireRequest{ParentPath: parent, ChildPath: child, MountPath: mount, StoreRoot: children, LeaseID: "lease-recovery"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := SnapshotUnixLease(lease)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := RecoverUnixLease(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recovered.Release(context.Background(), true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(child); !os.IsNotExist(err) {
		t.Fatalf("child not removed: %v", err)
	}
}

func TestUnixLeaseRecoveryRefusesReplacedMount(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "parent")
	children := filepath.Join(root, "children")
	child := filepath.Join(children, "lease-mount")
	mount := filepath.Join(root, "Library")
	_ = os.MkdirAll(parent, 0700)
	_ = os.MkdirAll(children, 0700)
	_ = os.WriteFile(filepath.Join(parent, "value"), []byte("parent"), 0600)
	lease, _, err := NewBackend().Acquire(context.Background(), AcquireRequest{ParentPath: parent, ChildPath: child, MountPath: mount, StoreRoot: children, LeaseID: "lease-mount"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _ := SnapshotUnixLease(lease)
	if err := os.Remove(mount); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(parent, mount); err != nil {
		t.Fatal(err)
	}
	if _, err := RecoverUnixLease(snapshot); err == nil {
		t.Fatal("replaced mount accepted")
	}
}
