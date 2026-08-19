//go:build darwin || linux

package workspaced

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	v2 "github.com/Kubonsang/unity-workspace-storage/contract/v2"
	"github.com/Kubonsang/unity-workspace-storage/storage"
	"golang.org/x/sys/unix"
)

func DefaultSocketPath() string {
	if value := os.Getenv("UNITY_WORKSPACE_STORAGE_SOCKET"); value != "" {
		return value
	}
	if value := os.Getenv("XDG_RUNTIME_DIR"); value != "" {
		return filepath.Join(value, "unity-workspace-storage.sock")
	}
	cache, err := os.UserCacheDir()
	if err == nil {
		return filepath.Join(cache, "unity-workspace-storage", "daemon.sock")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("unity-workspace-storage-%d.sock", os.Getuid()))
}

type SocketClient struct{ Path string }

func DefaultClient() v2.Client { return SocketClient{Path: DefaultSocketPath()} }
func (c SocketClient) Call(ctx context.Context, request v2.Request) (v2.Response, error) {
	path := c.Path
	if path == "" {
		path = DefaultSocketPath()
	}
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return v2.Response{}, err
	}
	defer connection.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	} else {
		_ = connection.SetDeadline(time.Now().Add(30 * time.Second))
	}
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return v2.Response{}, err
	}
	var response v2.Response
	if err := json.NewDecoder(bufio.NewReader(connection)).Decode(&response); err != nil {
		return response, err
	}
	return response, nil
}

func Serve(ctx context.Context, config Config) error {
	manager, err := NewManager(ctx, config, storage.NewBackend())
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(config.SocketPath), 0700); err != nil {
		return err
	}
	if info, err := os.Lstat(filepath.Dir(config.SocketPath)); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("socket directory must be a real directory")
	}
	lock, err := os.OpenFile(filepath.Join(config.StoreRoot, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fmt.Errorf("another daemon owns the store: %w", err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	if _, err := os.Lstat(config.SocketPath); err == nil {
		probe, probeErr := net.DialTimeout("unix", config.SocketPath, 250*time.Millisecond)
		if probeErr == nil {
			probe.Close()
			return errors.New("socket is already served")
		}
		if removeErr := os.Remove(config.SocketPath); removeErr != nil {
			return removeErr
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	listener, err := net.Listen("unix", config.SocketPath)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(config.SocketPath)
	if err := os.Chmod(config.SocketPath, 0600); err != nil {
		return err
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			listener.Close()
		case <-done:
		}
	}()
	defer close(done)
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go serveConnection(ctx, connection, manager)
	}
}

func serveConnection(ctx context.Context, connection net.Conn, manager *Manager) {
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(30 * time.Second))
	if err := authorizePeer(connection); err != nil {
		_ = json.NewEncoder(connection).Encode(v2.Response{SchemaVersion: 2, OK: false, Error: &v2.Error{Code: "unauthorized-client", Message: err.Error()}})
		return
	}
	var request v2.Request
	decoder := json.NewDecoder(bufio.NewReader(connection))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		_ = json.NewEncoder(connection).Encode(v2.Response{SchemaVersion: 2, OK: false, Error: &v2.Error{Code: "invalid-request", Message: err.Error()}})
		return
	}
	_ = json.NewEncoder(connection).Encode(manager.Handle(ctx, request))
}

func rawFD(connection net.Conn) (int, error) {
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return 0, errors.New("connection is not Unix")
	}
	raw, err := unixConnection.SyscallConn()
	if err != nil {
		return 0, err
	}
	var fd int
	var controlErr error
	if err := raw.Control(func(value uintptr) { fd = int(value) }); err != nil {
		return 0, err
	}
	return fd, controlErr
}
