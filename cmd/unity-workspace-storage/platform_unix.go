//go:build darwin || linux

package main

import (
	"context"
	"errors"
	"flag"
	"os/signal"
	"syscall"

	v2 "github.com/Kubonsang/unity-workspace-storage/contract/v2"
	"github.com/Kubonsang/unity-workspace-storage/internal/workspaced"
)

func defaultV2Client() v2.Client { return workspaced.DefaultClient() }

func platformServe(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := flags.String("config", "", "daemon config JSON")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *configPath == "" {
		return errors.Join(err, errors.New("serve requires --config"))
	}
	config, err := workspaced.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	serviceCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return workspaced.Serve(serviceCtx, config)
}
