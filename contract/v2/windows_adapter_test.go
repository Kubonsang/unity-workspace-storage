package v2

import (
	"context"
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

func TestWindowsAdapterKeepsParentProducerOnFrozenControlPlane(t *testing.T) {
	response, err := (WindowsAdapter{Client: legacyClientFunc(func(context.Context, workspace.Request) (workspace.Response, error) {
		t.Fatal("legacy client should not be called")
		return workspace.Response{}, nil
	})}).Call(context.Background(), NewRequest(OperationParentBegin, "windows-parent"))
	if err != nil {
		t.Fatal(err)
	}
	if response.Error == nil || response.Error.Code != "producer-control-plane-required" {
		t.Fatalf("response=%#v", response)
	}
}
