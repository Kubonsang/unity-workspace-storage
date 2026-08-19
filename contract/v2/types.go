// Package v2 defines the provider-neutral workspace storage contract.
package v2

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

const SchemaVersion = 2

const (
	OperationParentBegin  = "parent-begin"
	OperationParentCommit = "parent-commit"
	OperationParentAbort  = "parent-abort"
	OperationAcquire      = "workspace-acquire"
	OperationStatus       = "workspace-status"
	OperationRelease      = "workspace-release"
)

var sequence atomic.Uint64

type Error struct {
	Code      string `json:"code"`
	Operation string `json:"operation,omitempty"`
	Message   string `json:"message,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

type Limits struct {
	StoreMaxAllocatedBytes int64 `json:"storeMaxAllocatedBytes,omitempty"`
	MinimumHostFreeBytes   int64 `json:"minimumHostFreeBytes,omitempty"`
}

type Parent struct {
	ParentID       string    `json:"parentId"`
	Compatibility  string    `json:"compatibilityKey"`
	Provider       string    `json:"provider"`
	ArtifactKind   string    `json:"artifactKind"`
	ContentDigest  string    `json:"contentDigest"`
	LogicalBytes   int64     `json:"logicalBytes"`
	AllocatedBytes int64     `json:"allocatedBytes"`
	Immutable      bool      `json:"immutable"`
	CreatedAt      time.Time `json:"createdAt"`
}

type Lease struct {
	LeaseID       string    `json:"leaseId"`
	ConsumerID    string    `json:"consumerId"`
	WorkspaceID   string    `json:"workspaceId"`
	ParentID      string    `json:"parentId"`
	WorkspacePath string    `json:"workspacePath"`
	MountPath     string    `json:"mountPath"`
	State         string    `json:"state"`
	CreatedAt     time.Time `json:"createdAt"`
}

type Metrics struct {
	AcquireWallClockMs       int64 `json:"acquireWallClockMs,omitempty"`
	ReleaseWallClockMs       int64 `json:"releaseWallClockMs,omitempty"`
	ChildCreateMs            int64 `json:"childCreateMs,omitempty"`
	ChildReadyLogicalBytes   int64 `json:"childReadyLogicalBytes,omitempty"`
	ChildReadyAllocatedBytes int64 `json:"childReadyAllocatedBytes,omitempty"`
	CleanupMs                int64 `json:"cleanupMs,omitempty"`
}

type Capability struct {
	Platform          string `json:"platform"`
	Provider          string `json:"provider"`
	ArtifactKind      string `json:"artifactKind"`
	CoWAvailable      bool   `json:"cowAvailable"`
	RequiresElevation bool   `json:"requiresElevation"`
	Transport         string `json:"transport"`
	Error             string `json:"error,omitempty"`
}

type Status struct {
	Capability             Capability `json:"capability"`
	ParentCount            int        `json:"parentCount"`
	ActiveLeaseCount       int        `json:"activeLeaseCount"`
	QuarantineCount        int        `json:"quarantineCount"`
	AllocatedBytes         int64      `json:"allocatedBytes"`
	QuotaBytes             int64      `json:"quotaBytes"`
	HostFreeBytes          int64      `json:"hostFreeBytes"`
	HostFloorBytes         int64      `json:"hostFloorBytes"`
	ManualRecoveryRequired bool       `json:"manualRecoveryRequired"`
}

type Request struct {
	SchemaVersion    int    `json:"schemaVersion"`
	Operation        string `json:"operation"`
	RequestID        string `json:"requestId"`
	CompatibilityKey string `json:"compatibilityKey,omitempty"`
	TransactionID    string `json:"transactionId,omitempty"`
	ConsumerID       string `json:"consumerId,omitempty"`
	WorkspaceID      string `json:"workspaceId,omitempty"`
	ParentID         string `json:"parentId,omitempty"`
	LeaseID          string `json:"leaseId,omitempty"`
	ClientPID        int    `json:"clientPid,omitempty"`
	Limits           Limits `json:"limits,omitempty"`
}

type Response struct {
	SchemaVersion int      `json:"schemaVersion"`
	RequestID     string   `json:"requestId"`
	OK            bool     `json:"ok"`
	Provider      string   `json:"provider,omitempty"`
	TransactionID string   `json:"transactionId,omitempty"`
	StagingPath   string   `json:"stagingPath,omitempty"`
	Parent        *Parent  `json:"parent,omitempty"`
	Lease         *Lease   `json:"lease,omitempty"`
	Metrics       *Metrics `json:"metrics,omitempty"`
	Status        *Status  `json:"status,omitempty"`
	Error         *Error   `json:"error,omitempty"`
}

type Client interface {
	Call(context.Context, Request) (Response, error)
}

type Service struct{ Client Client }

func New(client Client) Service { return Service{Client: client} }

func NewRequest(operation, requestID string) Request {
	if requestID == "" {
		requestID = fmt.Sprintf("external-%s-%d-%d", operation, time.Now().UnixNano(), sequence.Add(1))
	}
	return Request{SchemaVersion: SchemaVersion, Operation: operation, RequestID: requestID}
}

func (s Service) call(ctx context.Context, request Request) (Response, error) {
	if s.Client == nil {
		return Response{}, errors.New("workspace storage daemon unavailable")
	}
	response, err := s.Client.Call(ctx, request)
	if err != nil {
		return response, err
	}
	if !response.OK {
		if response.Error != nil {
			return response, response.Error
		}
		return response, errors.New("workspace storage request failed")
	}
	return response, nil
}

type ParentBeginRequest struct{ RequestID, CompatibilityKey string }
type ParentCommitRequest struct{ RequestID, TransactionID string }
type ParentAbortRequest struct{ RequestID, TransactionID string }
type AcquireRequest struct {
	RequestID, ConsumerID, WorkspaceID, ParentID string
	ClientPID                                    int
	Limits                                       Limits
}
type StatusRequest struct{ RequestID string }
type ReleaseRequest struct{ RequestID, LeaseID string }

func (s Service) ParentBegin(ctx context.Context, input ParentBeginRequest) (Response, error) {
	r := NewRequest(OperationParentBegin, input.RequestID)
	r.CompatibilityKey = input.CompatibilityKey
	return s.call(ctx, r)
}
func (s Service) ParentCommit(ctx context.Context, input ParentCommitRequest) (Response, error) {
	r := NewRequest(OperationParentCommit, input.RequestID)
	r.TransactionID = input.TransactionID
	return s.call(ctx, r)
}
func (s Service) ParentAbort(ctx context.Context, input ParentAbortRequest) (Response, error) {
	r := NewRequest(OperationParentAbort, input.RequestID)
	r.TransactionID = input.TransactionID
	return s.call(ctx, r)
}
func (s Service) Acquire(ctx context.Context, input AcquireRequest) (Response, error) {
	r := NewRequest(OperationAcquire, input.RequestID)
	r.ConsumerID, r.WorkspaceID, r.ParentID, r.ClientPID, r.Limits = input.ConsumerID, input.WorkspaceID, input.ParentID, input.ClientPID, input.Limits
	return s.call(ctx, r)
}
func (s Service) Status(ctx context.Context, input StatusRequest) (Response, error) {
	return s.call(ctx, NewRequest(OperationStatus, input.RequestID))
}
func (s Service) Release(ctx context.Context, input ReleaseRequest) (Response, error) {
	r := NewRequest(OperationRelease, input.RequestID)
	r.LeaseID = input.LeaseID
	return s.call(ctx, r)
}
