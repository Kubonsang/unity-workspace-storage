//go:build windows

package fileusage

import "io/fs"

func allocatedFileBytes(info fs.FileInfo) int64 {
	return info.Size()
}
