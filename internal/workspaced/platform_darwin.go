//go:build darwin

package workspaced

import (
	"fmt"
	"golang.org/x/sys/unix"
	"syscall"
)

func platformBootID() string {
	value, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return ""
	}
	return fmt.Sprintf("boot-%d-%d", value.Sec, value.Usec)
}
func hostFreeBytes(path string) (int64, error) {
	var value syscall.Statfs_t
	if err := syscall.Statfs(path, &value); err != nil {
		return 0, err
	}
	return int64(value.Bavail) * int64(value.Bsize), nil
}
