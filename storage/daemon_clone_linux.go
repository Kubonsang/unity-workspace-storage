//go:build linux

package storage

import "context"

func cloneTreeForDaemon(ctx context.Context, source, destination string) error {
	return cloneTree(ctx, source, destination)
}
