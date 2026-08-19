package v2

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kubonsang/unity-workspace-storage/workspace"
)

type legacyClientFunc func(context.Context, workspace.Request) (workspace.Response, error)

func (fn legacyClientFunc) Call(ctx context.Context, request workspace.Request) (workspace.Response, error) {
	return fn(ctx, request)
}

func TestWindowsAdapterMapsLifecycleWithoutChangingFrozenWireSchema(t *testing.T) {
	adapter := WindowsAdapter{Client: legacyClientFunc(func(_ context.Context, request workspace.Request) (workspace.Response, error) {
		if request.SchemaVersion != workspace.ProtocolSchemaVersion || request.Operation != workspace.OperationAcquire || request.ParentKey == nil || request.ParentKey.Digest != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
			t.Fatalf("request=%#v", request)
		}
		return workspace.Response{OK: true, Provider: workspace.Provider, Lease: &workspace.Lease{LeaseID: "lease-win", RunID: request.RunID, ParentKey: request.ParentKey.Digest, MountPath: `C:\workspaces\ws\Library`, State: "ready"}}, nil
	})}
	request := NewRequest(OperationAcquire, "windows-acquire")
	request.ConsumerID = "consumer-win"
	request.WorkspaceID = "ws-win"
	request.ParentID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	response, err := adapter.Call(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !response.OK || response.Lease == nil || response.Lease.WorkspaceID != "ws-win" {
		t.Fatalf("response=%#v", response)
	}
}

func TestWindowsAdapterMapsParentProducerToFrozenControlPlane(t *testing.T) {
	root := t.TempDir()
	calls := 0
	adapter := WindowsAdapter{Client: legacyClientFunc(func(_ context.Context, request workspace.Request) (workspace.Response, error) {
		calls++
		switch request.Operation {
		case workspace.OperationHello:
			return workspace.Response{SchemaVersion: workspace.ProtocolSchemaVersion, OK: true, WorkspaceRoot: root}, nil
		case workspace.OperationBeginParentBuild:
			if request.ParentKey == nil || request.ParentKey.Digest != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" || request.Source == nil || request.WorkspaceID == "" {
				t.Fatalf("begin request=%#v", request)
			}
			mount := filepath.Join(root, request.WorkspaceID, "Library")
			return workspace.Response{SchemaVersion: workspace.ProtocolSchemaVersion, OK: true, Provider: workspace.Provider, ParentBuild: &workspace.ParentBuild{TransactionID: "parent-txn", MountPath: mount, State: "mounted"}}, nil
		default:
			t.Fatalf("unexpected operation %s", request.Operation)
			return workspace.Response{}, nil
		}
	})}
	request := NewRequest(OperationParentBegin, "windows-parent")
	request.CompatibilityKey = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	response, err := adapter.Call(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !response.OK || response.TransactionID != "parent-txn" || response.StagingPath == "" || calls != 2 {
		t.Fatalf("response=%#v", response)
	}
	if info, err := os.Lstat(filepath.Dir(response.StagingPath)); err != nil || !info.IsDir() {
		t.Fatalf("staging workspace missing: info=%v err=%v", info, err)
	}
}

func TestWindowsAdapterMapsParentCommitAndRejectsUnsupportedSchema(t *testing.T) {
	adapter := WindowsAdapter{Client: legacyClientFunc(func(_ context.Context, request workspace.Request) (workspace.Response, error) {
		if request.Operation != workspace.OperationCommitParent || request.TransactionID != "parent-txn" {
			t.Fatalf("request=%#v", request)
		}
		return workspace.Response{SchemaVersion: workspace.ProtocolSchemaVersion, OK: true, Provider: workspace.Provider, Parent: &workspace.ParentMetadata{CompatibilityKey: workspace.CompatibilityKey{Digest: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}, Provider: workspace.Provider, Immutable: true}}, nil
	})}
	commit := NewRequest(OperationParentCommit, "windows-commit")
	commit.TransactionID = "parent-txn"
	response, err := adapter.Call(context.Background(), commit)
	if err != nil || !response.OK || response.Parent == nil {
		t.Fatalf("commit response=%#v err=%v", response, err)
	}
	invalid := commit
	invalid.SchemaVersion = 99
	response, err = adapter.Call(context.Background(), invalid)
	if err != nil || response.Error == nil || response.Error.Code != "unsupported-schema" {
		t.Fatalf("unsupported response=%#v err=%v", response, err)
	}
}
