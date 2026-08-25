//go:build !windows

package workspace

import "context"

const WindowsServiceName = "UnityWorkspaceStorage"

func RunWindowsService(string) error                 { return ErrBrokerUnavailable }
func RunBrokerConsole(context.Context, string) error { return ErrBrokerUnavailable }
