//go:build windows

package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// PrepareRetainedStaleMount retains the strict stale-mount gate. A legacy
// journal GUID may be superseded only by evidence obtained from the exact,
// still-owned child image, opened read-only and without a drive letter.
func PrepareRetainedStaleMount(ctx context.Context, child, parent, mount, expected, fileID string) (bool, error) {
	removed, err := PrepareDetachedStaleMount(ctx, child, mount, expected)
	var cause *Error
	if err == nil || !errors.As(err, &cause) || cause.Operation != "validate-stale-mount-target" {
		return removed, err
	}
	actual, queryErr := retainedMountTarget(mount)
	if queryErr != nil {
		return false, errors.Join(err, queryErr)
	}
	if proofErr := proveRetainedVolume(ctx, child, parent, actual, fileID); proofErr != nil {
		return false, errors.Join(err, proofErr)
	}
	// Recheck after the proof handle has detached, including the exact link,
	// VHDX file identity and its detached state. Never remove arbitrary links.
	identity, identityErr := FileIdentity(child)
	if identityErr != nil || fileID == "" || identity != fileID {
		return false, errors.Join(err, identityErr, fmt.Errorf("child identity changed during recovery"))
	}
	return PrepareDetachedStaleMount(ctx, child, mount, actual)
}

func retainedMountTarget(mount string) (string, error) {
	ptr, err := windows.UTF16PtrFromString(ensureTrailingSeparator(mount))
	if err != nil {
		return "", err
	}
	buffer := make([]uint16, 1024)
	ok, _, callErr := procGetVolumeNameForMountPoint.Call(uintptr(unsafe.Pointer(ptr)), uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)))
	if ok == 0 {
		return "", callErr
	}
	return windows.UTF16ToString(buffer), nil
}

func proveRetainedVolume(ctx context.Context, child, parent, target, fileID string) (resultErr error) {
	identity, err := FileIdentity(child)
	if err != nil || fileID == "" || identity != fileID {
		return errors.Join(err, fmt.Errorf("retained child identity mismatch"))
	}
	query := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", detachedImageQueryScript)
	query.Env = append(os.Environ(), "TESTPLAY_VHDX_IMAGE_PATH="+child)
	output, err := query.CombinedOutput()
	if err != nil {
		return fmt.Errorf("query retained image: %w", err)
	}
	var image struct {
		ImagePath string `json:"imagePath"`
		Attached  bool   `json:"attached"`
	}
	if err := json.Unmarshal(output, &image); err != nil {
		return err
	}
	if image.Attached || !strings.EqualFold(filepath.Clean(image.ImagePath), filepath.Clean(child)) {
		return fmt.Errorf("retained image is already attached or has a different path")
	}
	attachment, err := Open(child, true)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, attachment.Close(context.Background())) }()
	if err := attachment.VerifyParent(parent); err != nil {
		return err
	}
	if err := attachment.Attach(true); err != nil {
		return err
	}
	if _, err := attachment.ResolvePhysicalPath(); err != nil {
		return err
	}
	volume, _, err := attachment.ResolveVolume(ctx, true)
	if err != nil {
		return err
	}
	if !sameVolumeGUID(volume.VolumeGUIDPath, target) {
		return fmt.Errorf("retained image resolves to %q, not mount target %q", volume.VolumeGUIDPath, target)
	}
	return nil
}
