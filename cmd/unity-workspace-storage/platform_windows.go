//go:build windows

package main

import (
	"context"
	"errors"
	v2 "github.com/Kubonsang/unity-workspace-storage/contract/v2"
	"github.com/Kubonsang/unity-workspace-storage/workspace"
)

func defaultV2Client() v2.Client { return v2.WindowsAdapter{Client: workspace.DefaultClient()} }
func platformServe(context.Context, []string) error {
	return errors.New("serve is Unix-only; Windows uses the installed named-pipe broker")
}
