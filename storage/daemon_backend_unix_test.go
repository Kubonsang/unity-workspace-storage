//go:build darwin || linux

package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDaemonNativeCoWTwoChildIsolationAndRelease(t *testing.T) {
	requireNativeCoWTest(t)
	root := resolvedTempDir(t)
	parent := filepath.Join(root, "parent")
	children := filepath.Join(root, "children")
	if err := os.MkdirAll(parent, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(children, 0700); err != nil {
		t.Fatal(err)
	}
	parentFile := filepath.Join(parent, "payload")
	if err := os.WriteFile(parentFile, []byte("parent"), 0600); err != nil {
		t.Fatal(err)
	}

	backend := NewDaemonBackend()
	acquire := func(name string) (Lease, string) {
		t.Helper()
		child := filepath.Join(children, name)
		mount := filepath.Join(root, name+"-Library")
		lease, metrics, err := backend.Acquire(context.Background(), AcquireRequest{ParentPath: parent, ChildPath: child, MountPath: mount, StoreRoot: children, LeaseID: name}, nil)
		if err != nil {
			t.Fatalf("required native CoW capability is unavailable for %s: %v", backend.Provider(), err)
		}
		if metrics.ChildReadyAllocatedBytes == nil || metrics.ChildReadyLogicalBytes == nil {
			t.Fatalf("missing allocated/logical byte evidence: %#v", metrics)
		}
		return lease, filepath.Join(mount, "payload")
	}

	first, firstFile := acquire("lease-first")
	second, secondFile := acquire("lease-second")
	if err := os.WriteFile(firstFile, []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, parentFile, "parent")
	assertFileContent(t, secondFile, "parent")
	if err := os.WriteFile(secondFile, []byte("second"), 0600); err != nil {
		t.Fatal(err)
	}
	assertFileContent(t, parentFile, "parent")
	assertFileContent(t, firstFile, "first")

	for _, lease := range []Lease{first, second} {
		if _, err := lease.Release(context.Background(), true, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(lease.Info().ChildPath); !os.IsNotExist(err) {
			t.Fatalf("child remains after clean release: %s: %v", lease.Info().ChildPath, err)
		}
	}
}

func assertFileContent(t *testing.T, path, expected string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != expected {
		t.Fatalf("%s=%q, want %q", path, data, expected)
	}
}
