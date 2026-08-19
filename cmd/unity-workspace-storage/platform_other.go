//go:build !windows && !darwin && !linux

package main

import (
	"context"
	"errors"
	v2 "github.com/Kubonsang/unity-workspace-storage/contract/v2"
)

type unavailableV2Client struct{}

func (unavailableV2Client) Call(context.Context, v2.Request) (v2.Response, error) {
	return v2.Response{}, errors.New("unsupported platform")
}
func defaultV2Client() v2.Client                    { return unavailableV2Client{} }
func platformServe(context.Context, []string) error { return errors.New("unsupported platform") }
