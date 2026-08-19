package v2

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Kubonsang/unity-workspace-storage/internal/atomicfile"
	"github.com/Kubonsang/unity-workspace-storage/workspace"
)

const windowsRequestClaimSchema = 1

var windowsRequestIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type windowsRequestClaim struct {
	SchemaVersion int    `json:"schemaVersion"`
	RequestID     string `json:"requestId"`
	Fingerprint   string `json:"fingerprint"`
}

// WindowsAdapter preserves the frozen broker while exposing the
// provider-neutral lifecycle. It maps schema-2 parent and workspace requests
// onto the broker's existing operations without changing the broker wire
// representation.
type WindowsAdapter struct {
	Client          workspace.Client
	RequestStateDir string
}

func (a WindowsAdapter) Call(ctx context.Context, request Request) (Response, error) {
	if a.Client == nil {
		return Response{}, workspace.ErrBrokerUnavailable
	}
	if request.SchemaVersion != SchemaVersion {
		return Response{SchemaVersion: SchemaVersion, RequestID: request.RequestID, OK: false, Provider: workspace.Provider, Error: &Error{Code: "unsupported-schema", Operation: request.Operation, Message: fmt.Sprintf("schemaVersion=%d", request.SchemaVersion)}}, nil
	}
	if !windowsRequestIdentifier.MatchString(request.RequestID) {
		return Response{SchemaVersion: SchemaVersion, RequestID: request.RequestID, OK: false, Provider: workspace.Provider, Error: &Error{Code: "invalid-request", Operation: request.Operation, Message: "invalid requestId"}}, nil
	}
	if code, err := a.claimRequest(request); err != nil {
		return Response{SchemaVersion: SchemaVersion, RequestID: request.RequestID, OK: false, Provider: workspace.Provider, Error: &Error{Code: code, Operation: request.Operation, Message: err.Error()}}, nil
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
		workspacePath, workspaceID, pathErr := windowsWorkspaceFromMount(response.Lease.MountPath)
		if pathErr != nil {
			result.OK = false
			result.Error = &Error{Code: "invalid-provider-response", Operation: request.Operation, Message: pathErr.Error()}
		} else if request.Operation == OperationAcquire && (workspaceID != request.WorkspaceID || response.Lease.RunID != request.ConsumerID || response.Lease.ParentKey != request.ParentID) {
			result.OK = false
			result.Error = &Error{Code: "duplicate-request-id", Operation: request.Operation, Message: "Windows broker replayed a response for a different schema-2 request"}
		} else {
			result.Lease = &Lease{LeaseID: response.Lease.LeaseID, ConsumerID: response.Lease.RunID, WorkspaceID: workspaceID, ParentID: response.Lease.ParentKey, WorkspacePath: workspacePath, MountPath: response.Lease.MountPath, State: response.Lease.State, CreatedAt: response.Lease.CreatedAt}
		}
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

func (a WindowsAdapter) claimRequest(request Request) (string, error) {
	encoded, err := json.Marshal(request)
	if err != nil {
		return "request-state-unavailable", err
	}
	fingerprintBytes := sha256.Sum256(encoded)
	fingerprint := hex.EncodeToString(fingerprintBytes[:])
	directory := a.RequestStateDir
	if directory == "" {
		cache, cacheErr := os.UserCacheDir()
		if cacheErr != nil {
			return "request-state-unavailable", cacheErr
		}
		directory = filepath.Join(cache, "unity-workspace-storage", "schema2-windows-requests")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return "request-state-unavailable", err
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "request-state-unavailable", errors.Join(err, errors.New("request state directory must be a real directory"))
	}
	nameBytes := sha256.Sum256([]byte(request.RequestID))
	path := filepath.Join(directory, hex.EncodeToString(nameBytes[:])+".json")
	claim := windowsRequestClaim{SchemaVersion: windowsRequestClaimSchema, RequestID: request.RequestID, Fingerprint: fingerprint}
	data, err := json.Marshal(claim)
	if err != nil {
		return "request-state-unavailable", err
	}
	if err := atomicfile.WriteExclusiveDurable(path, append(data, '\n'), 0600); err == nil {
		return "", nil
	} else if !os.IsExist(err) {
		return "request-state-unavailable", err
	}
	for attempt := 0; attempt < 50; attempt++ {
		existing, readErr := readWindowsRequestClaim(path)
		if readErr == nil {
			if existing.RequestID != request.RequestID || existing.Fingerprint != fingerprint {
				return "duplicate-request-id", errors.New("requestId was already used with a different payload")
			}
			return "", nil
		}
		if attempt == 49 {
			return "request-state-unavailable", readErr
		}
		time.Sleep(10 * time.Millisecond)
	}
	return "request-state-unavailable", errors.New("request claim did not become readable")
}

func readWindowsRequestClaim(path string) (windowsRequestClaim, error) {
	var claim windowsRequestClaim
	info, err := os.Lstat(path)
	if err != nil {
		return claim, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return claim, errors.New("request claim must be a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return claim, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&claim); err != nil {
		return claim, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return claim, errors.New("request claim contains trailing JSON")
	}
	if claim.SchemaVersion != windowsRequestClaimSchema || !windowsRequestIdentifier.MatchString(claim.RequestID) || len(claim.Fingerprint) != sha256.Size*2 {
		return claim, errors.New("request claim identity mismatch")
	}
	return claim, nil
}

func windowsWorkspaceFromMount(mount string) (string, string, error) {
	trimmed := strings.TrimRight(mount, `/\\`)
	separator := strings.LastIndexAny(trimmed, `/\\`)
	if separator <= 0 || !strings.EqualFold(trimmed[separator+1:], "Library") {
		return "", "", fmt.Errorf("invalid Windows workspace mount path: %q", mount)
	}
	workspacePath := strings.TrimRight(trimmed[:separator], `/\\`)
	workspaceSeparator := strings.LastIndexAny(workspacePath, `/\\`)
	if workspaceSeparator < 0 || workspaceSeparator == len(workspacePath)-1 {
		return "", "", fmt.Errorf("invalid Windows workspace path: %q", workspacePath)
	}
	workspaceID := workspacePath[workspaceSeparator+1:]
	if !windowsRequestIdentifier.MatchString(workspaceID) {
		return "", "", fmt.Errorf("invalid Windows workspace ID in mount path: %q", workspaceID)
	}
	return workspacePath, workspaceID, nil
}
