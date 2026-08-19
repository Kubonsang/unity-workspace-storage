//go:build darwin || linux

package workspaced

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	v2 "github.com/Kubonsang/unity-workspace-storage/contract/v2"
	"github.com/Kubonsang/unity-workspace-storage/storage"
)

type fakeBackend struct{}

func (fakeBackend) Platform() string                         { return "test" }
func (fakeBackend) Provider() string                         { return "test-cow" }
func (fakeBackend) Supported() bool                          { return true }
func (fakeBackend) RequiresElevation() bool                  { return false }
func (fakeBackend) IsElevated(context.Context) (bool, error) { return false, nil }
func (fakeBackend) Acquire(_ context.Context, r storage.AcquireRequest, _ storage.ProgressFunc) (storage.Lease, storage.Metrics, error) {
	if err := os.MkdirAll(r.ChildPath, 0700); err != nil {
		return nil, storage.Metrics{}, err
	}
	data, err := os.ReadFile(filepath.Join(r.ParentPath, "probe"))
	if err != nil {
		return nil, storage.Metrics{}, err
	}
	if err := os.WriteFile(filepath.Join(r.ChildPath, "probe"), data, 0600); err != nil {
		return nil, storage.Metrics{}, err
	}
	if err := os.Symlink(r.ChildPath, r.MountPath); err != nil {
		return nil, storage.Metrics{}, err
	}
	return &fakeLease{info: storage.LeaseInfo{ParentPath: r.ParentPath, ChildPath: r.ChildPath, MountPath: r.MountPath}}, storage.Metrics{}, nil
}

type fakeLease struct{ info storage.LeaseInfo }

func (l *fakeLease) Info() storage.LeaseInfo { return l.info }
func (l *fakeLease) Release(_ context.Context, deleteChild bool, _ storage.ProgressFunc) (storage.Metrics, error) {
	if err := os.Remove(l.info.MountPath); err != nil && !os.IsNotExist(err) {
		return storage.Metrics{}, err
	}
	if deleteChild {
		return storage.Metrics{}, os.RemoveAll(l.info.ChildPath)
	}
	return storage.Metrics{}, nil
}

func testConfig(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	store := filepath.Join(root, "store")
	workspaces := filepath.Join(root, "workspaces")
	socket := filepath.Join(root, "runtime", "daemon.sock")
	return Config{SchemaVersion: 1, StoreRoot: store, WorkspaceRoot: workspaces, SocketPath: socket, QuotaBytes: 1 << 30, HostFloorBytes: 0, ChildReserveBytes: 1}
}
func request(operation, id string) v2.Request {
	return v2.Request{SchemaVersion: 2, Operation: operation, RequestID: id}
}

func TestManagerParentAcquireStatusRelease(t *testing.T) {
	ctx := context.Background()
	manager, err := NewManager(ctx, testConfig(t), fakeBackend{})
	if err != nil {
		t.Fatal(err)
	}
	manager.snapshotLease = func(lease storage.Lease) (storage.UnixLeaseSnapshot, error) {
		info := lease.Info()
		return storage.UnixLeaseSnapshot{ParentPath: info.ParentPath, ChildPath: info.ChildPath, MountPath: info.MountPath, LeaseID: filepath.Base(info.ChildPath)}, nil
	}
	key := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	begin := request(v2.OperationParentBegin, "begin-1")
	begin.CompatibilityKey = key
	begun := manager.Handle(ctx, begin)
	if !begun.OK || begun.StagingPath == "" {
		t.Fatalf("begin=%#v", begun)
	}
	if err := os.WriteFile(filepath.Join(begun.StagingPath, "probe"), []byte("parent"), 0600); err != nil {
		t.Fatal(err)
	}
	commit := request(v2.OperationParentCommit, "commit-1")
	commit.TransactionID = begun.TransactionID
	committed := manager.Handle(ctx, commit)
	if !committed.OK || committed.Parent == nil {
		t.Fatalf("commit=%#v", committed)
	}
	workspace := filepath.Join(manager.config.WorkspaceRoot, "ws-1")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	acquire := request(v2.OperationAcquire, "acquire-1")
	acquire.ConsumerID = "consumer-1"
	acquire.WorkspaceID = "ws-1"
	acquire.ParentID = committed.Parent.ParentID
	acquired := manager.Handle(ctx, acquire)
	if !acquired.OK || acquired.Lease == nil {
		t.Fatalf("acquire=%#v", acquired)
	}
	if repeated := manager.Handle(ctx, acquire); repeated.Lease == nil || repeated.Lease.LeaseID != acquired.Lease.LeaseID {
		t.Fatalf("idempotency changed: %#v", repeated)
	}
	status := manager.Handle(ctx, request(v2.OperationStatus, "status-1"))
	if !status.OK || status.Status == nil || status.Status.ParentCount != 1 || status.Status.ActiveLeaseCount != 1 {
		t.Fatalf("status=%#v", status)
	}
	release := request(v2.OperationRelease, "release-1")
	release.LeaseID = acquired.Lease.LeaseID
	released := manager.Handle(ctx, release)
	if !released.OK {
		t.Fatalf("release=%#v", released)
	}
	if _, err := os.Lstat(workspace); !os.IsNotExist(err) {
		t.Fatalf("workspace not removed: %v", err)
	}
}

func TestManagerRejectsCorruptParentAndCapacity(t *testing.T) {
	ctx := context.Background()
	config := testConfig(t)
	manager, err := NewManager(ctx, config, fakeBackend{})
	if err != nil {
		t.Fatal(err)
	}
	key := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	begin := request(v2.OperationParentBegin, "begin-corrupt")
	begin.CompatibilityKey = key
	begun := manager.Handle(ctx, begin)
	_ = os.WriteFile(filepath.Join(begun.StagingPath, "probe"), []byte("before"), 0600)
	commit := request(v2.OperationParentCommit, "commit-corrupt")
	commit.TransactionID = begun.TransactionID
	parent := manager.Handle(ctx, commit).Parent
	_ = os.WriteFile(filepath.Join(config.StoreRoot, "parents", parent.ParentID, "data", "probe"), []byte("after"), 0600)
	_ = os.MkdirAll(filepath.Join(config.WorkspaceRoot, "ws-corrupt"), 0700)
	acquire := request(v2.OperationAcquire, "acquire-corrupt")
	acquire.ConsumerID = "consumer"
	acquire.WorkspaceID = "ws-corrupt"
	acquire.ParentID = parent.ParentID
	response := manager.Handle(ctx, acquire)
	if response.Error == nil || response.Error.Code != "parent-corrupt" {
		t.Fatalf("response=%#v", response)
	}
	config2 := testConfig(t)
	config2.QuotaBytes = 1
	limited, err := NewManager(ctx, config2, fakeBackend{})
	if err != nil {
		t.Fatal(err)
	}
	limitedBegin := request(v2.OperationParentBegin, "limited-begin")
	limitedBegin.CompatibilityKey = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	limitedBegun := limited.Handle(ctx, limitedBegin)
	_ = os.WriteFile(filepath.Join(limitedBegun.StagingPath, "probe"), []byte("larger-than-one-byte"), 0600)
	limitedCommit := request(v2.OperationParentCommit, "limited-commit")
	limitedCommit.TransactionID = limitedBegun.TransactionID
	limitedParent := limited.Handle(ctx, limitedCommit).Parent
	_ = os.MkdirAll(filepath.Join(config2.WorkspaceRoot, "limited-ws"), 0700)
	limitedAcquire := request(v2.OperationAcquire, "limited-acquire")
	limitedAcquire.ConsumerID = "limited-consumer"
	limitedAcquire.WorkspaceID = "limited-ws"
	limitedAcquire.ParentID = limitedParent.ParentID
	limitedResponse := limited.Handle(ctx, limitedAcquire)
	if limitedResponse.Error == nil || limitedResponse.Error.Code != "storage-capacity-unavailable" {
		t.Fatalf("response=%#v", limitedResponse)
	}
}

func TestManagerFailClosedOnUnrecoverableLease(t *testing.T) {
	manager, err := NewManager(context.Background(), testConfig(t), fakeBackend{})
	if err != nil {
		t.Fatal(err)
	}
	manager.recoverLease = func(storage.UnixLeaseSnapshot) (storage.Lease, error) { return nil, errors.New("identity changed") }
	journal := leaseJournal{Lease: v2.Lease{LeaseID: "lease-bad", WorkspacePath: filepath.Join(manager.config.WorkspaceRoot, "ws-bad")}, Snapshot: &storage.UnixLeaseSnapshot{LeaseID: "lease-bad"}}
	if err := writeJSON(filepath.Join(manager.config.StoreRoot, "leases", "lease-bad.json"), journal); err != nil {
		t.Fatal(err)
	}
	if err := manager.recover(context.Background()); err == nil {
		t.Fatal("expected recovery failure")
	}
	if _, err := os.Stat(filepath.Join(manager.config.StoreRoot, "leases", "lease-bad.json")); err != nil {
		t.Fatalf("journal was deleted: %v", err)
	}
}

func TestNativeManagerRestartRecoversLiveLease(t *testing.T) {
	ctx := context.Background()
	config := testConfig(t)
	first, err := NewManager(ctx, config, storage.NewBackend())
	if err != nil {
		t.Fatal(err)
	}
	if !first.capability.CoWAvailable {
		t.Fatalf("native CoW unavailable: %s", first.capability.Error)
	}
	key := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	begin := request(v2.OperationParentBegin, "native-begin")
	begin.CompatibilityKey = key
	begun := first.Handle(ctx, begin)
	if !begun.OK {
		t.Fatal(begun.Error)
	}
	if err := os.WriteFile(filepath.Join(begun.StagingPath, "probe"), []byte("native-parent"), 0600); err != nil {
		t.Fatal(err)
	}
	commit := request(v2.OperationParentCommit, "native-commit")
	commit.TransactionID = begun.TransactionID
	parent := first.Handle(ctx, commit).Parent
	if err := os.MkdirAll(filepath.Join(config.WorkspaceRoot, "native-ws"), 0700); err != nil {
		t.Fatal(err)
	}
	acquire := request(v2.OperationAcquire, "native-acquire")
	acquire.ConsumerID = "native-consumer"
	acquire.WorkspaceID = "native-ws"
	acquire.ParentID = parent.ParentID
	acquire.ClientPID = os.Getpid()
	acquired := first.Handle(ctx, acquire)
	if !acquired.OK {
		t.Fatal(acquired.Error)
	}
	second, err := NewManager(ctx, config, storage.NewBackend())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := second.leases[acquired.Lease.LeaseID]; !ok {
		t.Fatal("live lease was not reconstructed")
	}
	release := request(v2.OperationRelease, "native-release")
	release.LeaseID = acquired.Lease.LeaseID
	if response := second.Handle(ctx, release); !response.OK {
		t.Fatal(response.Error)
	}
}

func TestNativeManagerRestartCleansDeadClientLease(t *testing.T) {
	ctx := context.Background()
	config := testConfig(t)
	first, err := NewManager(ctx, config, storage.NewBackend())
	if err != nil {
		t.Fatal(err)
	}
	if !first.capability.CoWAvailable {
		t.Fatalf("native CoW unavailable: %s", first.capability.Error)
	}
	key := "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	begin := request(v2.OperationParentBegin, "dead-begin")
	begin.CompatibilityKey = key
	begun := first.Handle(ctx, begin)
	_ = os.WriteFile(filepath.Join(begun.StagingPath, "probe"), []byte("dead-parent"), 0600)
	commit := request(v2.OperationParentCommit, "dead-commit")
	commit.TransactionID = begun.TransactionID
	parent := first.Handle(ctx, commit).Parent
	_ = os.MkdirAll(filepath.Join(config.WorkspaceRoot, "dead-ws"), 0700)
	acquire := request(v2.OperationAcquire, "dead-acquire")
	acquire.ConsumerID = "dead-consumer"
	acquire.WorkspaceID = "dead-ws"
	acquire.ParentID = parent.ParentID
	acquire.ClientPID = 2147483647
	acquired := first.Handle(ctx, acquire)
	if !acquired.OK {
		t.Fatal(acquired.Error)
	}
	second, err := NewManager(ctx, config, storage.NewBackend())
	if err != nil {
		t.Fatal(err)
	}
	status := second.measureStatus()
	if status.ActiveLeaseCount != 0 {
		t.Fatalf("dead lease remains: %#v", status)
	}
}
