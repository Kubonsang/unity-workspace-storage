//go:build !windows

package workspace

func DefaultClient() Client { return unavailableClient{} }
