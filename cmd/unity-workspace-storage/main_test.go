package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Kubonsang/unity-workspace-storage/contract"
	v2 "github.com/Kubonsang/unity-workspace-storage/contract/v2"
)

type fakeLifecycle struct {
	acquire contract.AcquireRequest
	release contract.ReleaseRequest
}

type v2ClientFunc func(context.Context, v2.Request) (v2.Response, error)

func (fn v2ClientFunc) Call(ctx context.Context, request v2.Request) (v2.Response, error) {
	return fn(ctx, request)
}

func TestTopLevelRoutesSchema2AcquireWithoutChangingSchema1(t *testing.T) {
	legacy := &fakeLifecycle{}
	modern := v2.New(v2ClientFunc(func(_ context.Context, request v2.Request) (v2.Response, error) {
		if request.Operation != v2.OperationAcquire || request.ParentID != "parent-abc" {
			t.Fatalf("request=%#v", request)
		}
		return v2.Response{SchemaVersion: 2, RequestID: request.RequestID, OK: true, Lease: &v2.Lease{LeaseID: "lease-v2"}}, nil
	}))
	input := `{"schemaVersion":2,"operation":"workspace-acquire","requestId":"v2-acquire","consumerId":"honeybee","workspaceId":"ws-v2","parentId":"parent-abc"}`
	_, result, err := executeTop(context.Background(), []string{"workspace", "acquire"}, strings.NewReader(input), legacy, modern)
	if err != nil {
		t.Fatal(err)
	}
	response := result.(v2.Response)
	if response.Lease == nil || response.Lease.LeaseID != "lease-v2" {
		t.Fatalf("response=%#v", response)
	}
	if legacy.acquire.ConsumerID != "" {
		t.Fatal("schema 2 request reached schema 1 service")
	}
}

func TestTopLevelRoutesSchema2StatusFlag(t *testing.T) {
	modern := v2.New(v2ClientFunc(func(_ context.Context, request v2.Request) (v2.Response, error) {
		return v2.Response{SchemaVersion: 2, RequestID: request.RequestID, OK: true, Status: &v2.Status{}}, nil
	}))
	_, result, err := executeTop(context.Background(), []string{"workspace", "status", "--schema", "2", "--request-id", "status-v2"}, strings.NewReader(""), &fakeLifecycle{}, modern)
	if err != nil {
		t.Fatal(err)
	}
	if result.(v2.Response).Status == nil {
		t.Fatal("missing status")
	}
}

func TestCommandErrorPreservesSchema2ResponseAndStableCode(t *testing.T) {
	response := v2.Response{SchemaVersion: 2, RequestID: "release-v2", Error: &v2.Error{Code: "lease-not-found", Operation: v2.OperationRelease, Message: "missing"}}
	value := commandErrorValue([]string{"workspace", "release", "--schema", "2"}, "release", response, response.Error)
	actual, ok := value.(v2.Response)
	if !ok || actual.SchemaVersion != 2 || actual.Error == nil || actual.Error.Code != "lease-not-found" {
		t.Fatalf("value=%#v", value)
	}
}

func TestCommandParseErrorUsesRequestedSchema(t *testing.T) {
	value := commandErrorValue([]string{"parent", "begin"}, "parent-begin", nil, errors.New("missing key"))
	actual, ok := value.(cliError)
	if !ok || actual.SchemaVersion != 2 {
		t.Fatalf("value=%#v", value)
	}
}

func TestTopLevelSchema2DecodeErrorReturnsTypedResponse(t *testing.T) {
	input := `{"schemaVersion":2,"operation":"workspace-acquire","requestId":"bad-v2","unexpected":true}`
	operation, result, err := executeTop(context.Background(), []string{"workspace", "acquire"}, strings.NewReader(input), &fakeLifecycle{}, v2.Service{})
	if err == nil {
		t.Fatal("expected decode error")
	}
	response, ok := result.(v2.Response)
	if !ok || response.SchemaVersion != 2 || response.RequestID != "bad-v2" || response.Error == nil || response.Error.Code != "invalid-request" {
		t.Fatalf("operation=%s response=%#v", operation, result)
	}
	formatted, ok := commandErrorValue([]string{"workspace", "acquire"}, operation, result, err).(v2.Response)
	if !ok || formatted.SchemaVersion != 2 || formatted.Error == nil || formatted.Error.Code != "invalid-request" {
		t.Fatalf("formatted=%#v", formatted)
	}
}

func TestTopLevelTruncatedSchema2RequestReturnsTypedResponse(t *testing.T) {
	input := `{"schemaVersion":2,"requestId":"truncated-v2","consumerId":`
	operation, result, err := executeTop(context.Background(), []string{"workspace", "acquire"}, strings.NewReader(input), &fakeLifecycle{}, v2.Service{})
	if err == nil {
		t.Fatal("expected syntax error")
	}
	response, ok := result.(v2.Response)
	if !ok || response.SchemaVersion != 2 || response.RequestID != "truncated-v2" || response.Error == nil || response.Error.Code != "invalid-request" {
		t.Fatalf("operation=%s response=%#v", operation, result)
	}
}

func (fake *fakeLifecycle) Acquire(_ context.Context, request contract.AcquireRequest) (contract.AcquireResponse, error) {
	fake.acquire = request
	return contract.AcquireResponse{SchemaVersion: contract.SchemaVersion}, nil
}

func (*fakeLifecycle) Status(_ context.Context, request contract.StatusRequest) (contract.StatusResponse, error) {
	return contract.StatusResponse{SchemaVersion: request.SchemaVersion, RequestID: request.RequestID}, nil
}

func (fake *fakeLifecycle) Release(_ context.Context, request contract.ReleaseRequest) (contract.ReleaseResponse, error) {
	fake.release = request
	return contract.ReleaseResponse{SchemaVersion: contract.SchemaVersion}, nil
}

func TestAcquireReadsConsumerContractJSON(t *testing.T) {
	fake := &fakeLifecycle{}
	input := `{"schemaVersion":1,"requestId":"honeybee-acquire-1","consumerId":"honeybee-job-1","workspaceId":"honeybee-workspace-1","parentKey":{"schemaVersion":2,"digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","libraryKey":{},"provider":"vhdx-differencing","filesystem":"NTFS","virtualBytes":68719476736,"blockBytes":2097152,"sectorBytes":4096}}`
	operation, _, err := execute(context.Background(), []string{"workspace", "acquire"}, strings.NewReader(input), fake)
	if err != nil {
		t.Fatal(err)
	}
	if operation != "acquire" || fake.acquire.ConsumerID != "honeybee-job-1" || fake.acquire.WorkspaceID != "honeybee-workspace-1" {
		t.Fatalf("request mapping changed: %#v", fake.acquire)
	}
}

func TestReleaseUsesPublicLeaseFlags(t *testing.T) {
	fake := &fakeLifecycle{}
	_, _, err := execute(context.Background(), []string{"workspace", "release", "--lease-id", "lease-1"}, strings.NewReader(""), fake)
	if err != nil {
		t.Fatal(err)
	}
	if fake.release.SchemaVersion != contract.SchemaVersion || fake.release.LeaseID != "lease-1" {
		t.Fatalf("release mapping changed: %#v", fake.release)
	}
}
