package v2

import (
	"context"
	"testing"
)

type clientFunc func(context.Context, Request) (Response, error)

func (fn clientFunc) Call(ctx context.Context, request Request) (Response, error) {
	return fn(ctx, request)
}

func TestServiceMapsProviderNeutralAcquire(t *testing.T) {
	service := New(clientFunc(func(_ context.Context, request Request) (Response, error) {
		if request.SchemaVersion != 2 || request.Operation != OperationAcquire || request.ParentID != "linux-reflink:abc" || request.ConsumerID != "honeybee-1" {
			t.Fatalf("unexpected request: %#v", request)
		}
		return Response{SchemaVersion: 2, RequestID: request.RequestID, OK: true, Provider: "linux-reflink", Lease: &Lease{LeaseID: "lease-1"}}, nil
	}))
	response, err := service.Acquire(context.Background(), AcquireRequest{ConsumerID: "honeybee-1", WorkspaceID: "ws-1", ParentID: "linux-reflink:abc"})
	if err != nil || response.Lease == nil || response.Lease.LeaseID != "lease-1" {
		t.Fatalf("response=%#v err=%v", response, err)
	}
}

func TestServicePreservesStructuredError(t *testing.T) {
	service := New(clientFunc(func(_ context.Context, request Request) (Response, error) {
		return Response{SchemaVersion: 2, RequestID: request.RequestID, Error: &Error{Code: "parent-not-found", Message: "missing"}}, nil
	}))
	_, err := service.Acquire(context.Background(), AcquireRequest{ParentID: "missing"})
	if value, ok := err.(*Error); !ok || value.Code != "parent-not-found" {
		t.Fatalf("err=%T %v", err, err)
	}
}
