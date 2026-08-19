//go:build darwin || linux

package storage

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// UnixLeaseSnapshot is the durable, ownership-sensitive state required to
// reconstruct a directory-clone lease after the daemon restarts.
type UnixLeaseSnapshot struct {
	StoreRoot    string `json:"storeRoot"`
	ParentPath   string `json:"parentPath"`
	ChildPath    string `json:"childPath"`
	MountPath    string `json:"mountPath"`
	LeaseID      string `json:"leaseId"`
	OwnerToken   string `json:"ownerToken"`
	Device       uint64 `json:"device"`
	Inode        uint64 `json:"inode"`
	MountExisted bool   `json:"mountExisted"`
	MountMode    uint32 `json:"mountMode"`
}

func SnapshotUnixLease(lease Lease) (UnixLeaseSnapshot, error) {
	value, ok := lease.(*unixLease)
	if !ok || value == nil {
		return UnixLeaseSnapshot{}, fmt.Errorf("lease is not a Unix CoW lease")
	}
	return UnixLeaseSnapshot{
		StoreRoot: value.ownership.storeRoot, ParentPath: value.info.ParentPath,
		ChildPath: value.info.ChildPath, MountPath: value.info.MountPath,
		LeaseID: value.ownership.leaseID, OwnerToken: value.ownership.ownerToken,
		Device: value.ownership.identity.device, Inode: value.ownership.identity.inode,
		MountExisted: value.mountExisted, MountMode: uint32(value.mountMode.Perm()),
	}, nil
}

// RecoverUnixLease fails closed unless the durable identity, ownership marker,
// and workspace symlink still designate the exact acquired objects.
func RecoverUnixLease(snapshot UnixLeaseSnapshot) (Lease, error) {
	ownership := childOwnership{storeRoot: snapshot.StoreRoot, childPath: snapshot.ChildPath, leaseID: snapshot.LeaseID, ownerToken: snapshot.OwnerToken, identity: fileIdentity{device: snapshot.Device, inode: snapshot.Inode}}
	if err := validateOwnedChildAt(ownership, snapshot.ChildPath); err != nil {
		return nil, err
	}
	info, err := os.Lstat(snapshot.MountPath)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return nil, newError(CodeMountOwnershipLost, "recover-mount", snapshot.MountPath, err)
	}
	target, err := os.Readlink(snapshot.MountPath)
	if err != nil || filepath.Clean(target) != filepath.Clean(snapshot.ChildPath) {
		return nil, newError(CodeMountOwnershipLost, "recover-mount-target", snapshot.MountPath, err)
	}
	return &unixLease{
		info:      LeaseInfo{ParentPath: snapshot.ParentPath, ChildPath: snapshot.ChildPath, MountPath: snapshot.MountPath},
		ownership: ownership, mountExisted: snapshot.MountExisted, mountMode: fs.FileMode(snapshot.MountMode),
	}, nil
}
