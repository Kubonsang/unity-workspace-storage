//go:build darwin

package storage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

func cloneTreeForDaemon(ctx context.Context, source, destination string) error {
	var directories []string
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := destination
		if relative != "." {
			target = filepath.Join(destination, relative)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if err := os.Mkdir(target, info.Mode().Perm()|0700); err != nil {
				return err
			}
			directories = append(directories, target)
			return nil
		}
		return cloneRegularFileForDaemon(path, target)
	})
	if err != nil {
		return err
	}
	for index := len(directories) - 1; index >= 0; index-- {
		sourcePath := source
		if directories[index] != destination {
			relative, err := filepath.Rel(destination, directories[index])
			if err != nil {
				return err
			}
			sourcePath = filepath.Join(source, relative)
		}
		info, err := os.Stat(sourcePath)
		if err != nil {
			return err
		}
		if err := os.Chmod(directories[index], info.Mode().Perm()); err != nil {
			return err
		}
		if err := os.Chtimes(directories[index], info.ModTime(), info.ModTime()); err != nil {
			return err
		}
	}
	return nil
}

func cloneRegularFileForDaemon(source, destination string) error {
	// validateCloneSource rejects source-tree symlinks. Avoiding
	// CLONE_NOFOLLOW_ANY here permits absolute paths that traverse harmless
	// system aliases such as macOS /var -> /private/var.
	err := unix.Clonefileat(unix.AT_FDCWD, source, unix.AT_FDCWD, destination, unix.CLONE_NOOWNERCOPY)
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.EXDEV) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EINVAL) {
		return fmt.Errorf("%w: clonefileat: %v", errCoWUnavailable, err)
	}
	return fmt.Errorf("clonefileat: %w", err)
}
