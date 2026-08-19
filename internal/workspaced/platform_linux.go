//go:build linux

package workspaced

import (
	"os"
	"strings"
	"syscall"
)

func platformBootID() string {
	data, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(data))
}
func hostFreeBytes(path string) (int64, error) {
	var value syscall.Statfs_t
	if err := syscall.Statfs(path, &value); err != nil {
		return 0, err
	}
	return int64(value.Bavail) * int64(value.Bsize), nil
}
