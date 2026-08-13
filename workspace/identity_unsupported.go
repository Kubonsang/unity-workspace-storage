//go:build !windows

package workspace

import "runtime"

func CurrentUserSID() (string, error) {
	return "", wrap("unsupported", "current-user-sid", runtime.GOOS, ErrBrokerUnavailable)
}

func IsElevated() bool { return false }
