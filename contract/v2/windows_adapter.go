package v2

import (
	"context"
	"errors"
	"strings"

	"github.com/Kubonsang/unity-workspace-storage/workspace"
)

// WindowsAdapter preserves the frozen schema-1 broker while exposing the
// provider-neutral lifecycle. Windows parent construction remains available
// through the frozen producer control plane; lifecycle callers use its
// compatibility digest as the opaque parent ID.
type WindowsAdapter struct{ Client workspace.Client }

func (a WindowsAdapter) Call(ctx context.Context, request Request) (Response, error) {
	if a.Client == nil {
		return Response{}, workspace.ErrBrokerUnavailable
	}
	legacy := workspace.NewRequest("", request.RequestID)
	switch request.Operation {
	case OperationAcquire:
		legacy.Operation = workspace.OperationAcquire
		legacy.RunID = request.ConsumerID
		legacy.WorkspaceID = request.WorkspaceID
		legacy.ClientPID = request.ClientPID
		legacy.ParentKey = &workspace.CompatibilityKey{SchemaVersion: workspace.ParentSchemaVersion, Digest: request.ParentID}
		legacy.StoreMaxAllocatedBytes = request.Limits.StoreMaxAllocatedBytes
		legacy.MinimumHostFreeBytes = request.Limits.MinimumHostFreeBytes
	case OperationStatus:
		legacy.Operation = workspace.OperationStatus
	case OperationRelease:
		legacy.Operation = workspace.OperationRelease
		legacy.LeaseID = request.LeaseID
	case OperationParentBegin, OperationParentCommit, OperationParentAbort:
		return Response{SchemaVersion: 2, RequestID: request.RequestID, OK: false, Provider: workspace.Provider, Error: &Error{Code: "producer-control-plane-required", Operation: request.Operation, Message: "Windows parent construction uses the frozen schema-1 producer control plane"}}, nil
	default:
		return Response{SchemaVersion: 2, RequestID: request.RequestID, OK: false, Error: &Error{Code: "unknown-operation", Operation: request.Operation}}, nil
	}
	response, err := a.Client.Call(ctx, legacy)
	if err != nil && response.Error == nil {
		return Response{}, err
	}
	result := Response{SchemaVersion: 2, RequestID: request.RequestID, OK: response.OK, Provider: response.Provider}
	if response.Error != nil {
		result.Error = &Error{Code: response.Error.Code, Operation: response.Error.Operation, Message: response.Error.Message}
	}
	if response.Parent != nil {
		result.Parent = &Parent{ParentID: response.Parent.CompatibilityKey.Digest, Compatibility: response.Parent.CompatibilityKey.Digest, Provider: response.Parent.Provider, ArtifactKind: "vhdx", ContentDigest: strings.ToLower(response.Parent.CommittedSHA256), LogicalBytes: response.Parent.LogicalBytes, AllocatedBytes: response.Parent.AllocatedBytes, Immutable: response.Parent.Immutable, CreatedAt: response.Parent.CreatedAt}
	}
	if response.Lease != nil {
		result.Lease = &Lease{LeaseID: response.Lease.LeaseID, ConsumerID: response.Lease.RunID, WorkspaceID: request.WorkspaceID, ParentID: response.Lease.ParentKey, MountPath: response.Lease.MountPath, State: response.Lease.State, CreatedAt: response.Lease.CreatedAt}
	}
	if response.Status != nil {
		result.Status = &Status{Capability: Capability{Platform: "windows", Provider: response.Provider, ArtifactKind: "vhdx", CoWAvailable: true, RequiresElevation: true, Transport: "named-pipe"}, ParentCount: response.Status.ParentCount, ActiveLeaseCount: response.Status.ActiveChildCount, QuarantineCount: response.Status.QuarantineCount, AllocatedBytes: response.Status.Capacity.AllocatedBytes, QuotaBytes: response.Status.Capacity.QuotaBytes, HostFreeBytes: response.Status.Capacity.HostFreeBytes, HostFloorBytes: response.Status.Capacity.HostFloorBytes, ManualRecoveryRequired: response.Status.ManualRecoveryRequired}
	}
	if !result.OK && result.Error == nil {
		return result, errors.New("Windows broker request failed")
	}
	return result, nil
}
