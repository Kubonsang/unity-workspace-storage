//go:build darwin || linux

package storage

// NewDaemonBackend returns the CoW backend used by the schema-2 Unix daemon.
// It is additive so the checkpoint-derived provider remains byte-for-byte
// frozen while the daemon can apply platform integration fixes independently.
func NewDaemonBackend() Backend {
	return unixBackend{clone: cloneTreeForDaemon}
}
