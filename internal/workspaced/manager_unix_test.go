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
		return storage.UnixLeaseSnapshot{StoreRoot: filepath.Dir(info.ChildPath), ParentPath: info.ParentPath, ChildPath: info.ChildPath, MountPath: info.MountPath, LeaseID: filepath.Base(info.ChildPath), OwnerToken: "storage-owner"}, nil
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
	projectFile := filepath.Join(workspace, "Assets", "keep.txt")
	if err := os.MkdirAll(filepath.Dir(projectFile), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectFile, []byte("producer-owned"), 0600); err != nil {
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
	conflict := acquire
	conflict.ParentID = "parent-different"
	if duplicate := manager.Handle(ctx, conflict); duplicate.Error == nil || duplicate.Error.Code != "duplicate-request-id" {
		t.Fatalf("duplicate requestId response=%#v", duplicate)
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
	data, err := os.ReadFile(projectFile)
	if err != nil || string(data) != "producer-owned" {
		t.Fatalf("producer workspace was modified: data=%q err=%v", data, err)
	}
	if _, err := os.Lstat(filepath.Join(workspace, workspaceOwnerFile)); !os.IsNotExist(err) {
		t.Fatalf("workspace owner marker remains: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(workspace, "Library")); !os.IsNotExist(err) {
		t.Fatalf("Library mount remains: %v", err)
	}
}

func TestParentCommitResumesPublicationAndUsesDurableReceipt(t *testing.T) {
	ctx := context.Background()
	manager, err := NewManager(ctx, testConfig(t), fakeBackend{})
	if err != nil {
		t.Fatal(err)
	}
	begin := request(v2.OperationParentBegin, "resume-begin")
	begin.CompatibilityKey = "abababababababababababababababababababababababababababababababab"
	begun := manager.Handle(ctx, begin)
	if !begun.OK {
		t.Fatal(begun.Error)
	}
	if err := os.WriteFile(filepath.Join(begun.StagingPath, "probe"), []byte("resumable"), 0600); err != nil {
		t.Fatal(err)
	}
	transactionDir := filepath.Dir(begun.StagingPath)
	publication := filepath.Join(transactionDir, "publication")
	if err := os.Mkdir(publication, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(begun.StagingPath, filepath.Join(publication, "data")); err != nil {
		t.Fatal(err)
	}
	commit := request(v2.OperationParentCommit, "resume-commit")
	commit.TransactionID = begun.TransactionID
	committed := manager.Handle(ctx, commit)
	if !committed.OK || committed.Parent == nil {
		t.Fatalf("commit=%#v", committed)
	}
	restarted, err := NewManager(ctx, manager.config, fakeBackend{})
	if err != nil {
		t.Fatal(err)
	}
	retry := request(v2.OperationParentCommit, "resume-commit-after-restart")
	retry.TransactionID = begun.TransactionID
	repeated := restarted.Handle(ctx, retry)
	if !repeated.OK || repeated.Parent == nil || repeated.Parent.ParentID != committed.Parent.ParentID {
		t.Fatalf("receipt retry=%#v", repeated)
	}
}

func TestConcurrentParentCommitRemovesRedundantStagingTree(t *testing.T) {
	ctx := context.Background()
	manager, err := NewManager(ctx, testConfig(t), fakeBackend{})
	if err != nil {
		t.Fatal(err)
	}
	key := "acacacacacacacacacacacacacacacacacacacacacacacacacacacacacacacac"
	first := request(v2.OperationParentBegin, "same-key-first")
	first.CompatibilityKey = key
	second := request(v2.OperationParentBegin, "same-key-second")
	second.CompatibilityKey = key
	firstBegun := manager.Handle(ctx, first)
	secondBegun := manager.Handle(ctx, second)
	for _, begun := range []v2.Response{firstBegun, secondBegun} {
		if !begun.OK {
			t.Fatalf("begin=%#v", begun)
		}
		if err := os.WriteFile(filepath.Join(begun.StagingPath, "probe"), []byte("same-parent"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	firstCommit := request(v2.OperationParentCommit, "same-key-first-commit")
	firstCommit.TransactionID = firstBegun.TransactionID
	if response := manager.Handle(ctx, firstCommit); !response.OK {
		t.Fatalf("first commit=%#v", response)
	}
	secondCommit := request(v2.OperationParentCommit, "same-key-second-commit")
	secondCommit.TransactionID = secondBegun.TransactionID
	if response := manager.Handle(ctx, secondCommit); !response.OK {
		t.Fatalf("second commit=%#v", response)
	}
	if _, err := os.Lstat(filepath.Dir(secondBegun.StagingPath)); !os.IsNotExist(err) {
		t.Fatalf("redundant pending tree remains: %v", err)
	}
}

func TestParentDigestRejectsEmptyDirectoryMutation(t *testing.T) {
	ctx := context.Background()
	config := testConfig(t)
	manager, err := NewManager(ctx, config, fakeBackend{})
	if err != nil {
		t.Fatal(err)
	}
	begin := request(v2.OperationParentBegin, "tree-begin")
	begin.CompatibilityKey = "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
	begun := manager.Handle(ctx, begin)
	_ = os.WriteFile(filepath.Join(begun.StagingPath, "probe"), []byte("parent"), 0600)
	commit := request(v2.OperationParentCommit, "tree-commit")
	commit.TransactionID = begun.TransactionID
	parent := manager.Handle(ctx, commit).Parent
	parentData := filepath.Join(config.StoreRoot, "parents", parent.ParentID, "data")
	if err := os.Mkdir(filepath.Join(parentData, "injected-empty-directory"), 0700); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(config.WorkspaceRoot, "tree-workspace")
	_ = os.MkdirAll(workspace, 0700)
	acquire := request(v2.OperationAcquire, "tree-acquire")
	acquire.ConsumerID = "tree-consumer"
	acquire.WorkspaceID = "tree-workspace"
	acquire.ParentID = parent.ParentID
	response := manager.Handle(ctx, acquire)
	if response.Error == nil || response.Error.Code != "parent-corrupt" {
		t.Fatalf("empty-directory mutation response=%#v", response)
	}
}

func TestParentDigestRejectsModeMutation(t *testing.T) {
	ctx := context.Background()
	config := testConfig(t)
	manager, err := NewManager(ctx, config, fakeBackend{})
	if err != nil {
		t.Fatal(err)
	}
	begin := request(v2.OperationParentBegin, "mode-begin")
	begin.CompatibilityKey = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	begun := manager.Handle(ctx, begin)
	if err := os.WriteFile(filepath.Join(begun.StagingPath, "probe"), []byte("parent"), 0600); err != nil {
		t.Fatal(err)
	}
	commit := request(v2.OperationParentCommit, "mode-commit")
	commit.TransactionID = begun.TransactionID
	parent := manager.Handle(ctx, commit).Parent
	probe := filepath.Join(config.StoreRoot, "parents", parent.ParentID, "data", "probe")
	if err := os.Chmod(probe, 0644); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(config.WorkspaceRoot, "mode-workspace")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	acquire := request(v2.OperationAcquire, "mode-acquire")
	acquire.ConsumerID = "mode-consumer"
	acquire.WorkspaceID = "mode-workspace"
	acquire.ParentID = parent.ParentID
	response := manager.Handle(ctx, acquire)
	if response.Error == nil || response.Error.Code != "parent-corrupt" {
		t.Fatalf("mode mutation response=%#v", response)
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
	workspacePath := filepath.Join(manager.config.WorkspaceRoot, "ws-bad")
	childPath := filepath.Join(manager.config.StoreRoot, "children", "lease-bad")
	mountPath := filepath.Join(workspacePath, "Library")
	parentPath := filepath.Join(manager.config.StoreRoot, "parents", "parent-bad", "data")
	if err := os.MkdirAll(childPath, 0700); err != nil {
		t.Fatal(err)
	}
	journal := leaseJournal{SchemaVersion: 2, Lease: v2.Lease{LeaseID: "lease-bad", ConsumerID: "consumer-bad", WorkspaceID: "ws-bad", ParentID: "parent-bad", WorkspacePath: workspacePath, MountPath: mountPath, State: "ready"}, ChildPath: childPath, ClientPID: os.Getpid(), BootSessionID: manager.bootID, OwnerToken: "workspace-owner", Snapshot: &storage.UnixLeaseSnapshot{StoreRoot: filepath.Join(manager.config.StoreRoot, "children"), ParentPath: parentPath, ChildPath: childPath, MountPath: mountPath, LeaseID: "lease-bad", OwnerToken: "storage-owner"}}
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

func TestAcquireInitialJournalFailureRemovesWorkspaceOwner(t *testing.T) {
	ctx := context.Background()
	manager, parentID := managerWithCommittedParent(t, ctx)
	workspacePath := filepath.Join(manager.config.WorkspaceRoot, "journal-fail-ws")
	if err := os.MkdirAll(workspacePath, 0700); err != nil {
		t.Fatal(err)
	}
	manager.writeLeaseJSON = func(string, any) error { return errors.New("injected journal failure") }
	acquire := request(v2.OperationAcquire, "journal-fail-acquire")
	acquire.ConsumerID = "journal-fail-consumer"
	acquire.WorkspaceID = "journal-fail-ws"
	acquire.ParentID = parentID
	response := manager.Handle(ctx, acquire)
	if response.Error == nil || response.Error.Code != "journal-write-failed" {
		t.Fatalf("response=%#v", response)
	}
	if _, err := os.Lstat(filepath.Join(workspacePath, workspaceOwnerFile)); !os.IsNotExist(err) {
		t.Fatalf("workspace owner marker remains: %v", err)
	}
}

func TestAcquireReadyJournalFailureRollsBackLease(t *testing.T) {
	ctx := context.Background()
	manager, parentID := managerWithCommittedParent(t, ctx)
	manager.snapshotLease = func(lease storage.Lease) (storage.UnixLeaseSnapshot, error) {
		info := lease.Info()
		return storage.UnixLeaseSnapshot{StoreRoot: filepath.Dir(info.ChildPath), ParentPath: info.ParentPath, ChildPath: info.ChildPath, MountPath: info.MountPath, LeaseID: filepath.Base(info.ChildPath), OwnerToken: "storage-owner"}, nil
	}
	writes := 0
	manager.writeLeaseJSON = func(path string, value any) error {
		writes++
		if writes == 2 {
			return errors.New("injected ready journal failure")
		}
		return writeJSON(path, value)
	}
	workspacePath := filepath.Join(manager.config.WorkspaceRoot, "ready-fail-ws")
	if err := os.MkdirAll(workspacePath, 0700); err != nil {
		t.Fatal(err)
	}
	acquire := request(v2.OperationAcquire, "ready-fail-acquire")
	acquire.ConsumerID = "ready-fail-consumer"
	acquire.WorkspaceID = "ready-fail-ws"
	acquire.ParentID = parentID
	response := manager.Handle(ctx, acquire)
	if response.Error == nil || response.Error.Code != "journal-write-failed" {
		t.Fatalf("response=%#v", response)
	}
	for _, path := range []string{filepath.Join(workspacePath, workspaceOwnerFile), filepath.Join(workspacePath, "Library")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("rollback path remains: %s err=%v", path, err)
		}
	}
	children, err := os.ReadDir(filepath.Join(manager.config.StoreRoot, "children"))
	if err != nil || len(children) != 0 {
		t.Fatalf("rollback child remains: entries=%v err=%v", children, err)
	}
	leases, err := os.ReadDir(filepath.Join(manager.config.StoreRoot, "leases"))
	if err != nil || len(leases) != 0 {
		t.Fatalf("rollback journal remains: entries=%v err=%v", leases, err)
	}
}

func TestRecoveryRejectsJournalOutsideConfiguredRoots(t *testing.T) {
	ctx := context.Background()
	manager, err := NewManager(ctx, testConfig(t), fakeBackend{})
	if err != nil {
		t.Fatal(err)
	}
	called := false
	manager.recoverLease = func(storage.UnixLeaseSnapshot) (storage.Lease, error) {
		called = true
		return nil, errors.New("must not be called")
	}
	workspacePath := filepath.Join(manager.config.WorkspaceRoot, "tampered-ws")
	childPath := filepath.Join(manager.config.StoreRoot, "children", "lease-tampered")
	mountPath := filepath.Join(workspacePath, "Library")
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(childPath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(outside, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	journal := leaseJournal{SchemaVersion: 2, Lease: v2.Lease{LeaseID: "lease-tampered", ConsumerID: "consumer-tampered", WorkspaceID: "tampered-ws", ParentID: "parent-tampered", WorkspacePath: workspacePath, MountPath: mountPath, State: "ready"}, ChildPath: childPath, ClientPID: os.Getpid(), BootSessionID: manager.bootID, OwnerToken: "workspace-owner", Snapshot: &storage.UnixLeaseSnapshot{StoreRoot: outside, ParentPath: filepath.Join(manager.config.StoreRoot, "parents", "parent-tampered", "data"), ChildPath: childPath, MountPath: mountPath, LeaseID: "lease-tampered", OwnerToken: "storage-owner"}}
	if err := writeJSON(filepath.Join(manager.config.StoreRoot, "leases", "lease-tampered.json"), journal); err != nil {
		t.Fatal(err)
	}
	if err := manager.recover(ctx); err == nil {
		t.Fatal("expected structural recovery validation failure")
	}
	if called {
		t.Fatal("untrusted snapshot reached storage recovery")
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "keep" {
		t.Fatalf("outside sentinel changed: data=%q err=%v", data, err)
	}
}

func managerWithCommittedParent(t *testing.T, ctx context.Context) (*Manager, string) {
	t.Helper()
	manager, err := NewManager(ctx, testConfig(t), fakeBackend{})
	if err != nil {
		t.Fatal(err)
	}
	begin := request(v2.OperationParentBegin, "helper-begin")
	begin.CompatibilityKey = "fafafafafafafafafafafafafafafafafafafafafafafafafafafafafafafafa"
	begun := manager.Handle(ctx, begin)
	if !begun.OK {
		t.Fatal(begun.Error)
	}
	if err := os.WriteFile(filepath.Join(begun.StagingPath, "probe"), []byte("parent"), 0600); err != nil {
		t.Fatal(err)
	}
	commit := request(v2.OperationParentCommit, "helper-commit")
	commit.TransactionID = begun.TransactionID
	committed := manager.Handle(ctx, commit)
	if !committed.OK || committed.Parent == nil {
		t.Fatalf("commit=%#v", committed)
	}
	return manager, committed.Parent.ParentID
}

func TestNativeManagerRestartRecoversLiveLease(t *testing.T) {
	requireNativeCoWTest(t)
	ctx := context.Background()
	config := testConfig(t)
	first, err := NewManager(ctx, config, storage.NewDaemonBackend())
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
	second, err := NewManager(ctx, config, storage.NewDaemonBackend())
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
	requireNativeCoWTest(t)
	ctx := context.Background()
	config := testConfig(t)
	first, err := NewManager(ctx, config, storage.NewDaemonBackend())
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
	workspace := filepath.Join(config.WorkspaceRoot, "dead-ws")
	projectFile := filepath.Join(workspace, "Assets", "survives.txt")
	if err := os.MkdirAll(filepath.Dir(projectFile), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectFile, []byte("producer-owned"), 0600); err != nil {
		t.Fatal(err)
	}
	acquire := request(v2.OperationAcquire, "dead-acquire")
	acquire.ConsumerID = "dead-consumer"
	acquire.WorkspaceID = "dead-ws"
	acquire.ParentID = parent.ParentID
	acquire.ClientPID = 2147483647
	acquired := first.Handle(ctx, acquire)
	if !acquired.OK {
		t.Fatal(acquired.Error)
	}
	second, err := NewManager(ctx, config, storage.NewDaemonBackend())
	if err != nil {
		t.Fatal(err)
	}
	status, err := second.measureStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.ActiveLeaseCount != 0 {
		t.Fatalf("dead lease remains: %#v", status)
	}
	data, err := os.ReadFile(projectFile)
	if err != nil || string(data) != "producer-owned" {
		t.Fatalf("orphan recovery modified producer workspace: data=%q err=%v", data, err)
	}
	if _, err := os.Lstat(filepath.Join(workspace, workspaceOwnerFile)); !os.IsNotExist(err) {
		t.Fatalf("workspace owner marker remains after recovery: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(workspace, "Library")); !os.IsNotExist(err) {
		t.Fatalf("Library mount remains after recovery: %v", err)
	}
}

func requireNativeCoWTest(t *testing.T) {
	t.Helper()
	if os.Getenv("UNITY_WORKSPACE_STORAGE_NATIVE_TEST") != "1" {
		t.Skip("native lifecycle runs in the platform capability job")
	}
}
