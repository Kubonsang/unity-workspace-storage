package contract

import (
	"context"
	"errors"
	"testing"

	"github.com/Kubonsang/unity-workspace-storage/workspace"
)

type clientFunc func(context.Context, workspace.Request) (workspace.Response, error)

func (fn clientFunc) Call(ctx context.Context, request workspace.Request) (workspace.Response, error) {
	return fn(ctx, request)
}

func TestAcquireMapsConsumerWithoutTestPlayConcepts(t *testing.T) {
	key := workspace.CompatibilityKey{Digest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	service := New(clientFunc(func(_ context.Context, request workspace.Request) (workspace.Response, error) {
		if request.Operation != workspace.OperationAcquire || request.RunID != "honeybee-job-17" || request.WorkspaceID != "honeybee-workspace-17" {
			t.Fatalf("unexpected broker request: %#v", request)
		}
		if request.ParentKey == nil || request.ParentKey.Digest != key.Digest || request.SchemaVersion != workspace.ProtocolSchemaVersion {
			t.Fatalf("parent/protocol mapping changed: %#v", request)
		}
		return workspace.Response{RequestID: request.RequestID, OK: true, Provider: workspace.Provider, Lease: &workspace.Lease{LeaseID: "lease-17", State: "ready"}}, nil
	}))
	result, err := service.Acquire(context.Background(), AcquireRequest{SchemaVersion: SchemaVersion, ConsumerID: "honeybee-job-17", WorkspaceID: "honeybee-workspace-17", ParentKey: key})
	if err != nil {
		t.Fatal(err)
	}
	if result.SchemaVersion != SchemaVersion || result.Lease == nil || result.Lease.LeaseID != "lease-17" {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestStatusAndReleaseMapExactly(t *testing.T) {
	var operations []string
	service := New(clientFunc(func(_ context.Context, request workspace.Request) (workspace.Response, error) {
		operations = append(operations, request.Operation)
		switch request.Operation {
		case workspace.OperationStatus:
			return workspace.Response{RequestID: request.RequestID, OK: true, Provider: workspace.Provider, Status: &workspace.Status{Provider: workspace.Provider}}, nil
		case workspace.OperationRelease:
			if request.LeaseID != "lease-42" || request.RetainChild {
				t.Fatalf("release mapping changed: %#v", request)
			}
			return workspace.Response{RequestID: request.RequestID, OK: true, Provider: workspace.Provider, Lease: &workspace.Lease{LeaseID: request.LeaseID}}, nil
		default:
			t.Fatalf("unexpected operation %q", request.Operation)
			return workspace.Response{}, nil
		}
	}))
	if _, err := service.Status(context.Background(), StatusRequest{SchemaVersion: SchemaVersion}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Release(context.Background(), ReleaseRequest{SchemaVersion: SchemaVersion, LeaseID: "lease-42"}); err != nil {
		t.Fatal(err)
	}
	if len(operations) != 2 || operations[0] != workspace.OperationStatus || operations[1] != workspace.OperationRelease {
		t.Fatalf("unexpected operations: %v", operations)
	}
}

func TestContractRejectsSchemaDriftAndPreservesTransportErrors(t *testing.T) {
	sentinel := errors.New("transport unavailable")
	service := New(clientFunc(func(context.Context, workspace.Request) (workspace.Response, error) {
		return workspace.Response{}, sentinel
	}))
	if _, err := service.Status(context.Background(), StatusRequest{}); err == nil {
		t.Fatal("expected schema rejection")
	}
	_, err := service.Status(context.Background(), StatusRequest{SchemaVersion: SchemaVersion})
	if !errors.Is(err, sentinel) {
		t.Fatalf("transport error lost: %v", err)
	}
}
