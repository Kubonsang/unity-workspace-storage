package main

import (
	"context"
	"strings"
	"testing"

	"github.com/Kubonsang/unity-workspace-storage/contract"
)

type fakeLifecycle struct {
	acquire contract.AcquireRequest
	release contract.ReleaseRequest
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
