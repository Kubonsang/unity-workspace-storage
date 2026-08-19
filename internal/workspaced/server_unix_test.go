//go:build darwin || linux

package workspaced

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v2 "github.com/Kubonsang/unity-workspace-storage/contract/v2"
)

func TestUnixSocketServerAuthenticatesCurrentUserAndServesStatus(t *testing.T) {
	config := testConfig(t)
	shortRuntime, err := os.MkdirTemp("/tmp", "uws-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shortRuntime) })
	config.SocketPath = filepath.Join(shortRuntime, "daemon.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, config) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Lstat(config.SocketPath); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("server stopped before creating socket: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("socket was not created")
		}
		time.Sleep(10 * time.Millisecond)
	}
	response, err := SocketClient{Path: config.SocketPath}.Call(context.Background(), v2.NewRequest(v2.OperationStatus, "socket-status"))
	if err != nil {
		t.Fatal(err)
	}
	if !response.OK || response.Status == nil || response.Status.Capability.Transport != "unix-socket" {
		t.Fatalf("response=%#v", response)
	}
	info, err := os.Stat(config.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("socket mode=%o", info.Mode().Perm())
	}
	if err := Serve(context.Background(), config); err == nil || !strings.Contains(err.Error(), "another daemon owns the store") {
		t.Fatalf("second daemon did not fail the store lock: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop")
	}
}
