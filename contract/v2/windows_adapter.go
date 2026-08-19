package v2

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kubonsang/unity-workspace-storage/workspace"
)

// WindowsAdapter preserves the frozen broker while exposing the
// provider-neutral lifecycle. It maps schema-2 parent and workspace requests
// onto the broker's existing operations without changing the broker wire
// representation.
type WindowsAdapter struct{ Client workspace.Client }

func (a WindowsAdapter) Call(ctx context.Context, request Request) (Response, error) {
	if a.Client == nil {
		return Response{}, workspace.ErrBrokerUnavailable
	}
	if request.SchemaVersion != SchemaVersion {
		return Response{SchemaVersion: SchemaVersion, RequestID: request.RequestID, OK: false, Provider: workspace.Provider, Error: &Error{Code: "unsupported-schema", Operation: request.Operation, Message: fmt.Sprintf("schemaVersion=%d", request.SchemaVersion)}}, nil
	}
	legacy := workspace.NewRequest("", request.RequestID)
	switch request.Operation {
	case OperationParentBegin:
		return a.beginParent(ctx, request)
	case OperationParentCommit:
		legacy.Operation = workspace.OperationCommitParent
		legacy.TransactionID = request.TransactionID
	case OperationParentAbort:
		legacy.Operation = workspace.OperationAbortParent
		legacy.TransactionID = request.TransactionID
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
	default:
		return Response{SchemaVersion: 2, RequestID: request.RequestID, OK: false, Error: &Error{Code: "unknown-operation", Operation: request.Operation}}, nil
	}
	response, err := a.Client.Call(ctx, legacy)
	return translateWindowsResponse(request, response, err)
}

func (a WindowsAdapter) beginParent(ctx context.Context, request Request) (Response, error) {
	if len(request.CompatibilityKey) != 64 {
		return Response{SchemaVersion: SchemaVersion, RequestID: request.RequestID, OK: false, Provider: workspace.Provider, Error: &Error{Code: "invalid-compatibility-key", Operation: request.Operation, Message: "compatibilityKey must be 64 lowercase hex characters"}}, nil
	}
	if _, err := hex.DecodeString(request.CompatibilityKey); err != nil || strings.ToLower(request.CompatibilityKey) != request.CompatibilityKey {
		return Response{SchemaVersion: SchemaVersion, RequestID: request.RequestID, OK: false, Provider: workspace.Provider, Error: &Error{Code: "invalid-compatibility-key", Operation: request.Operation, Message: "compatibilityKey must be 64 lowercase hex characters"}}, nil
	}
	hello := workspace.NewRequest(workspace.OperationHello, adapterRequestID("hello", request.RequestID))
	helloResponse, err := a.Client.Call(ctx, hello)
	if err != nil && helloResponse.Error == nil {
		return Response{}, err
	}
	if !helloResponse.OK || helloResponse.WorkspaceRoot == "" || !filepath.IsAbs(helloResponse.WorkspaceRoot) {
		translated, translateErr := translateWindowsResponse(request, helloResponse, err)
		if translated.Error == nil {
			translated.OK = false
			translated.Error = &Error{Code: "staging-unavailable", Operation: request.Operation, Message: "Windows broker did not return an absolute workspace root"}
		}
		return translated, translateErr
	}
	workspaceID := "schema2-parent-" + request.CompatibilityKey[:24]
	workspacePath := filepath.Join(helloResponse.WorkspaceRoot, workspaceID)
	if err := os.MkdirAll(workspacePath, 0700); err != nil {
		return Response{SchemaVersion: SchemaVersion, RequestID: request.RequestID, OK: false, Provider: workspace.Provider, Error: &Error{Code: "staging-create-failed", Operation: request.Operation, Message: err.Error()}}, nil
	}
	info, err := os.Lstat(workspacePath)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return Response{SchemaVersion: SchemaVersion, RequestID: request.RequestID, OK: false, Provider: workspace.Provider, Error: &Error{Code: "staging-create-failed", Operation: request.Operation, Message: "Windows staging workspace is not a real directory"}}, nil
	}
	legacy := workspace.NewRequest(workspace.OperationBeginParentBuild, request.RequestID)
	legacy.WorkspaceID = workspaceID
	legacy.ClientPID = request.ClientPID
	if legacy.ClientPID == 0 {
		legacy.ClientPID = os.Getpid()
	}
	legacy.ParentKey = &workspace.CompatibilityKey{SchemaVersion: workspace.ParentSchemaVersion, Digest: request.CompatibilityKey, Provider: workspace.Provider, Filesystem: "NTFS", VirtualBytes: workspace.DefaultVirtualBytes, BlockBytes: workspace.DefaultBlockBytes, SectorBytes: workspace.DefaultSectorBytes}
	legacy.Source = &workspace.SourceSnapshot{Digest: request.CompatibilityKey}
	response, callErr := a.Client.Call(ctx, legacy)
	return translateWindowsResponse(request, response, callErr)
}

func translateWindowsResponse(request Request, response workspace.Response, err error) (Response, error) {
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
	if response.ParentBuild != nil {
		result.TransactionID = response.ParentBuild.TransactionID
		result.StagingPath = response.ParentBuild.MountPath
		if response.ParentBuild.State == "waiting" {
			result.OK = false
			result.Error = &Error{Code: "parent-pending", Operation: request.Operation, Message: "another producer owns the parent transaction"}
		}
	}
	if response.Status != nil {
		result.Status = &Status{Capability: Capability{Platform: "windows", Provider: response.Provider, ArtifactKind: "vhdx", CoWAvailable: true, RequiresElevation: true, Transport: "named-pipe"}, ParentCount: response.Status.ParentCount, ActiveLeaseCount: response.Status.ActiveChildCount, QuarantineCount: response.Status.QuarantineCount, AllocatedBytes: response.Status.Capacity.AllocatedBytes, QuotaBytes: response.Status.Capacity.QuotaBytes, HostFreeBytes: response.Status.Capacity.HostFreeBytes, HostFloorBytes: response.Status.Capacity.HostFloorBytes, ManualRecoveryRequired: response.Status.ManualRecoveryRequired}
	}
	if !result.OK && result.Error == nil {
		return result, errors.New("Windows broker request failed")
	}
	return result, nil
}

func adapterRequestID(prefix, requestID string) string {
	sum := sha256.Sum256([]byte(prefix + "\x00" + requestID))
	return "schema2-" + prefix + "-" + hex.EncodeToString(sum[:8])
}
