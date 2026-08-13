//go:build !windows

package workspace

func validatePlatformRealDirectory(string) error { return nil }
func validatePlatformNonReparse(string) error    { return nil }
