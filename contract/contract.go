// Package contract exposes the consumer-neutral workspace lifecycle boundary.
// It deliberately maps only acquire, status, and release onto the frozen
// provider protocol; parent construction and broker administration remain
// separate operator responsibilities.
package contract

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/Kubonsang/unity-workspace-storage/workspace"
)

const SchemaVersion = 1

var requestSequence atomic.Uint64

// Service maps the public consumer contract to the extracted broker protocol.
// UserSID is optional: the Windows pipe authenticates the caller independently.
type Service struct {
	Client  workspace.Client
	UserSID string
}

func New(client workspace.Client) Service { return Service{Client: client} }

func Default() Service { return New(workspace.DefaultClient()) }

type AcquireRequest struct {
	SchemaVersion          int                        `json:"schemaVersion"`
	RequestID              string                     `json:"requestId,omitempty"`
	ConsumerID             string                     `json:"consumerId"`
	WorkspaceID            string                     `json:"workspaceId"`
	ParentKey              workspace.CompatibilityKey `json:"parentKey"`
	ClientPID              int                        `json:"clientPid,omitempty"`
	StoreMaxAllocatedBytes int64                      `json:"storeMaxAllocatedBytes,omitempty"`
	MinimumHostFreeBytes   int64                      `json:"minimumHostFreeBytes,omitempty"`
}

type AcquireResponse struct {
	SchemaVersion int                       `json:"schemaVersion"`
	RequestID     string                    `json:"requestId"`
	Provider      string                    `json:"provider"`
	Lease         *workspace.Lease          `json:"lease"`
	Parent        *workspace.ParentMetadata `json:"parent,omitempty"`
	Metrics       *workspace.Metrics        `json:"metrics,omitempty"`
}

type StatusRequest struct {
	SchemaVersion int    `json:"schemaVersion"`
	RequestID     string `json:"requestId,omitempty"`
}

type StatusResponse struct {
	SchemaVersion int               `json:"schemaVersion"`
	RequestID     string            `json:"requestId"`
	Provider      string            `json:"provider"`
	Status        *workspace.Status `json:"status"`
}

type ReleaseRequest struct {
	SchemaVersion int    `json:"schemaVersion"`
	RequestID     string `json:"requestId,omitempty"`
	LeaseID       string `json:"leaseId"`
}

type ReleaseResponse struct {
	SchemaVersion int                `json:"schemaVersion"`
	RequestID     string             `json:"requestId"`
	Provider      string             `json:"provider"`
	Lease         *workspace.Lease   `json:"lease,omitempty"`
	Metrics       *workspace.Metrics `json:"metrics,omitempty"`
}

func (service Service) Acquire(ctx context.Context, input AcquireRequest) (AcquireResponse, error) {
	if err := validateVersion(input.SchemaVersion); err != nil {
		return AcquireResponse{}, err
	}
	requestID := normalizedRequestID("acquire", input.RequestID)
	request := workspace.NewRequest(workspace.OperationAcquire, requestID)
	request.UserSID = service.UserSID
	request.RunID = input.ConsumerID
	request.WorkspaceID = input.WorkspaceID
	request.ParentKey = &input.ParentKey
	request.ClientPID = input.ClientPID
	request.StoreMaxAllocatedBytes = input.StoreMaxAllocatedBytes
	request.MinimumHostFreeBytes = input.MinimumHostFreeBytes
	response, err := service.call(ctx, request)
	if err != nil {
		return AcquireResponse{}, err
	}
	if response.Lease == nil {
		return AcquireResponse{}, fmt.Errorf("workspace acquire: broker returned no lease")
	}
	return AcquireResponse{SchemaVersion: SchemaVersion, RequestID: response.RequestID, Provider: response.Provider, Lease: response.Lease, Parent: response.Parent, Metrics: response.Metrics}, nil
}

func (service Service) Status(ctx context.Context, input StatusRequest) (StatusResponse, error) {
	if err := validateVersion(input.SchemaVersion); err != nil {
		return StatusResponse{}, err
	}
	request := workspace.NewRequest(workspace.OperationStatus, normalizedRequestID("status", input.RequestID))
	request.UserSID = service.UserSID
	response, err := service.call(ctx, request)
	if err != nil {
		return StatusResponse{}, err
	}
	if response.Status == nil {
		return StatusResponse{}, fmt.Errorf("workspace status: broker returned no status")
	}
	return StatusResponse{SchemaVersion: SchemaVersion, RequestID: response.RequestID, Provider: response.Provider, Status: response.Status}, nil
}

func (service Service) Release(ctx context.Context, input ReleaseRequest) (ReleaseResponse, error) {
	if err := validateVersion(input.SchemaVersion); err != nil {
		return ReleaseResponse{}, err
	}
	request := workspace.NewRequest(workspace.OperationRelease, normalizedRequestID("release", input.RequestID))
	request.UserSID = service.UserSID
	request.LeaseID = input.LeaseID
	response, err := service.call(ctx, request)
	if err != nil {
		return ReleaseResponse{}, err
	}
	return ReleaseResponse{SchemaVersion: SchemaVersion, RequestID: response.RequestID, Provider: response.Provider, Lease: response.Lease, Metrics: response.Metrics}, nil
}

func (service Service) call(ctx context.Context, request workspace.Request) (workspace.Response, error) {
	if service.Client == nil {
		return workspace.Response{}, workspace.ErrBrokerUnavailable
	}
	return service.Client.Call(ctx, request)
}

func validateVersion(version int) error {
	if version != SchemaVersion {
		return fmt.Errorf("unsupported public contract schemaVersion %d", version)
	}
	return nil
}

func normalizedRequestID(operation, value string) string {
	if value != "" {
		return value
	}
	return fmt.Sprintf("external-%s-%d-%d", operation, time.Now().UnixNano(), requestSequence.Add(1))
}
