//go:build windows

package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRetainedRecoveryRejectsInvalidFileIdentityBeforeOpeningImage(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "not-a-vhdx")
	if err := os.WriteFile(child, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "different-file-id"} {
		if err := proveRetainedVolume(context.Background(), child, "missing-parent", `\\?\Volume{other}\`, id); err == nil {
			t.Fatal("unverified child accepted")
		}
	}
	bytes, err := os.ReadFile(child)
	if err != nil || string(bytes) != "preserve" {
		t.Fatalf("child changed: %v", err)
	}
}

func TestRetainedRecoveryLeavesOrdinaryDirectoriesAndFilesUntouched(t *testing.T) {
	root := t.TempDir()
	mount := filepath.Join(root, "Library")
	if err := os.Mkdir(mount, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(mount, "authored.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	removed, err := PrepareRetainedStaleMount(context.Background(), filepath.Join(root, "child.vhdx"), filepath.Join(root, "parent.vhdx"), mount, `\\?\Volume{expected}\`, "unverified")
	if removed || err != nil {
		t.Fatalf("ordinary directory: removed=%v, err=%v", removed, err)
	}
	if bytes, err := os.ReadFile(marker); err != nil || string(bytes) != "keep" {
		t.Fatalf("ordinary files changed: %v", err)
	}
}
