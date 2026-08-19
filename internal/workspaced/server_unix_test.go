//go:build darwin || linux

package workspaced

import (
	"context"
	"os"
	"testing"
	"time"

	v2 "github.com/Kubonsang/unity-workspace-storage/contract/v2"
)

func TestUnixSocketServerAuthenticatesCurrentUserAndServesStatus(t *testing.T) {
	config := testConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, config) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Lstat(config.SocketPath); err == nil {
			break
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
