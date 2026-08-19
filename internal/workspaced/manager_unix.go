//go:build darwin || linux

package workspaced

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	v2 "github.com/Kubonsang/unity-workspace-storage/contract/v2"
	"github.com/Kubonsang/unity-workspace-storage/internal/atomicfile"
	"github.com/Kubonsang/unity-workspace-storage/internal/fileusage"
	"github.com/Kubonsang/unity-workspace-storage/storage"
)

const (
	configSchema          = 1
	workspaceOwnerFile    = ".unity-workspace-storage-owner.json"
	reservedStorageMarker = ".testplay-storage-owner"
)

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var compatibility = regexp.MustCompile(`^[a-f0-9]{64}$`)

type Config struct {
	SchemaVersion     int    `json:"schemaVersion"`
	StoreRoot         string `json:"storeRoot"`
	WorkspaceRoot     string `json:"workspaceRoot"`
	SocketPath        string `json:"socketPath"`
	QuotaBytes        int64  `json:"quotaBytes"`
	HostFloorBytes    int64  `json:"hostFloorBytes"`
	ChildReserveBytes int64  `json:"childReserveBytes"`
}

func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var config Config
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return Config{}, err
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func (c Config) Validate() error {
	if c.SchemaVersion != configSchema || !filepath.IsAbs(c.StoreRoot) || !filepath.IsAbs(c.WorkspaceRoot) || !filepath.IsAbs(c.SocketPath) {
		return fmt.Errorf("invalid daemon config")
	}
	if c.QuotaBytes <= 0 || c.HostFloorBytes < 0 || c.ChildReserveBytes <= 0 {
		return fmt.Errorf("invalid capacity config")
	}
	return nil
}

type parentRecord struct {
	v2.Parent
	DataPath string `json:"dataPath"`
	Device   uint64 `json:"device"`
	Inode    uint64 `json:"inode"`
}

type pendingRecord struct {
	TransactionID    string    `json:"transactionId"`
	CompatibilityKey string    `json:"compatibilityKey"`
	StagingPath      string    `json:"stagingPath"`
	CreatedAt        time.Time `json:"createdAt"`
}

type parentReceipt struct {
	TransactionID string `json:"transactionId"`
	ParentID      string `json:"parentId"`
}

type leaseJournal struct {
	v2.Lease
	ChildPath     string                     `json:"childPath"`
	ClientPID     int                        `json:"clientPid"`
	BootSessionID string                     `json:"bootSessionId"`
	OwnerToken    string                     `json:"ownerToken"`
	Snapshot      *storage.UnixLeaseSnapshot `json:"snapshot,omitempty"`
	UpdatedAt     time.Time                  `json:"updatedAt"`
}

type workspaceOwner struct {
	SchemaVersion int    `json:"schemaVersion"`
	LeaseID       string `json:"leaseId"`
	WorkspaceID   string `json:"workspaceId"`
	WorkspacePath string `json:"workspacePath"`
	MountPath     string `json:"mountPath"`
	OwnerToken    string `json:"ownerToken"`
}

type Manager struct {
	mu             sync.Mutex
	config         Config
	backend        storage.Backend
	bootID         string
	capability     v2.Capability
	leases         map[string]storage.Lease
	requests       map[string]requestRecord
	manualRecovery bool
	snapshotLease  func(storage.Lease) (storage.UnixLeaseSnapshot, error)
	recoverLease   func(storage.UnixLeaseSnapshot) (storage.Lease, error)
}

type requestRecord struct {
	Request  v2.Request
	Response v2.Response
}

func NewManager(ctx context.Context, config Config, backend storage.Backend) (*Manager, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if backend == nil {
		backend = storage.NewBackend()
	}
	for _, path := range []string{config.StoreRoot, config.WorkspaceRoot, filepath.Join(config.StoreRoot, "parents"), filepath.Join(config.StoreRoot, "pending"), filepath.Join(config.StoreRoot, "children"), filepath.Join(config.StoreRoot, "leases"), filepath.Join(config.StoreRoot, "quarantine"), filepath.Join(config.StoreRoot, "receipts")} {
		if err := os.MkdirAll(path, 0700); err != nil {
			return nil, err
		}
		if info, err := os.Lstat(path); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("unsafe real directory required: %s", path)
		}
	}
	m := &Manager{config: config, backend: backend, bootID: platformBootID(), leases: map[string]storage.Lease{}, requests: map[string]requestRecord{}, snapshotLease: storage.SnapshotUnixLease, recoverLease: storage.RecoverUnixLease}
	m.capability = v2.Capability{Platform: runtime.GOOS, Provider: backend.Provider(), ArtifactKind: "directory", RequiresElevation: false, Transport: "unix-socket"}
	if err := m.probe(ctx); err != nil {
		m.capability.Error = err.Error()
	} else {
		m.capability.CoWAvailable = true
	}
	if err := m.recover(ctx); err != nil {
		m.manualRecovery = true
	}
	return m, nil
}

func (m *Manager) Handle(ctx context.Context, request v2.Request) v2.Response {
	m.mu.Lock()
	defer m.mu.Unlock()
	if prior, ok := m.requests[request.RequestID]; ok {
		if prior.Request != request {
			return v2.Response{SchemaVersion: 2, RequestID: request.RequestID, OK: false, Provider: m.backend.Provider(), Error: &v2.Error{Code: "duplicate-request-id", Operation: request.Operation, Message: "requestId was already used with a different payload"}}
		}
		return prior.Response
	}
	response := m.handle(ctx, request)
	if request.RequestID != "" {
		m.requests[request.RequestID] = requestRecord{Request: request, Response: response}
	}
	return response
}

func (m *Manager) handle(ctx context.Context, request v2.Request) v2.Response {
	fail := func(code string, err error) v2.Response {
		return v2.Response{SchemaVersion: 2, RequestID: request.RequestID, OK: false, Provider: m.backend.Provider(), Error: &v2.Error{Code: code, Operation: request.Operation, Message: errorText(err)}}
	}
	if request.SchemaVersion != 2 {
		return fail("unsupported-schema", fmt.Errorf("schemaVersion=%d", request.SchemaVersion))
	}
	if !identifier.MatchString(request.RequestID) {
		return fail("invalid-request", fmt.Errorf("invalid requestId"))
	}
	switch request.Operation {
	case v2.OperationStatus:
		return m.status(request.RequestID)
	case v2.OperationParentBegin:
		return m.parentBegin(request, fail)
	case v2.OperationParentCommit:
		return m.parentCommit(request, fail)
	case v2.OperationParentAbort:
		return m.parentAbort(request, fail)
	case v2.OperationAcquire:
		if !m.capability.CoWAvailable {
			return fail(storage.CodeCoWUnavailable, errors.New(m.capability.Error))
		}
		return m.acquire(ctx, request, fail)
	case v2.OperationRelease:
		return m.release(ctx, request, fail)
	default:
		return fail("unknown-operation", fmt.Errorf("operation=%s", request.Operation))
	}
}

type failFunc func(string, error) v2.Response

func (m *Manager) parentBegin(request v2.Request, fail failFunc) v2.Response {
	if !compatibility.MatchString(request.CompatibilityKey) {
		return fail("invalid-compatibility-key", errors.New("compatibilityKey must be 64 lowercase hex characters"))
	}
	if parent, err := m.readParentByCompatibility(request.CompatibilityKey); err == nil {
		if verifyErr := verifyParent(parent); verifyErr == nil {
			return v2.Response{SchemaVersion: 2, RequestID: request.RequestID, OK: true, Provider: m.backend.Provider(), Parent: &parent.Parent}
		}
		if quarantineErr := m.quarantineParent(parent); quarantineErr != nil {
			m.manualRecovery = true
			return fail("parent-corrupt", quarantineErr)
		}
	} else {
		parentID := parentID(m.backend.Provider(), request.CompatibilityKey)
		if _, statErr := os.Lstat(filepath.Join(m.config.StoreRoot, "parents", parentID)); statErr == nil {
			if quarantineErr := m.quarantineParentID(parentID); quarantineErr != nil {
				m.manualRecovery = true
				return fail("parent-corrupt", errors.Join(err, quarantineErr))
			}
		} else if !os.IsNotExist(statErr) {
			m.manualRecovery = true
			return fail("parent-corrupt", errors.Join(err, statErr))
		}
	}
	txn, err := randomID("txn")
	if err != nil {
		return fail("transaction-create-failed", err)
	}
	dir := filepath.Join(m.config.StoreRoot, "pending", txn)
	staging := filepath.Join(dir, "data")
	if err := os.MkdirAll(staging, 0700); err != nil {
		return fail("transaction-create-failed", err)
	}
	pending := pendingRecord{TransactionID: txn, CompatibilityKey: request.CompatibilityKey, StagingPath: staging, CreatedAt: time.Now().UTC()}
	if err := writeJSON(filepath.Join(dir, "pending.json"), pending); err != nil {
		return fail("transaction-create-failed", err)
	}
	return v2.Response{SchemaVersion: 2, RequestID: request.RequestID, OK: true, Provider: m.backend.Provider(), TransactionID: txn, StagingPath: staging}
}

func (m *Manager) parentCommit(request v2.Request, fail failFunc) v2.Response {
	if !identifier.MatchString(request.TransactionID) {
		return fail("invalid-transaction", errors.New("invalid transactionId"))
	}
	receipt, receiptErr := m.readParentReceipt(request.TransactionID)
	if receiptErr == nil {
		parent, parentErr := m.readParent(receipt.ParentID)
		var verifyErr error
		if parentErr == nil {
			verifyErr = verifyParent(parent)
		}
		if parentErr != nil || verifyErr != nil {
			m.manualRecovery = true
			return fail("parent-corrupt", errors.Join(parentErr, verifyErr, errors.New("committed parent receipt is not valid")))
		}
		cleanupPendingTransaction(filepath.Join(m.config.StoreRoot, "pending", request.TransactionID))
		return v2.Response{SchemaVersion: 2, RequestID: request.RequestID, OK: true, Provider: m.backend.Provider(), Parent: &parent.Parent}
	}
	if !os.IsNotExist(receiptErr) {
		m.manualRecovery = true
		return fail("parent-receipt-corrupt", receiptErr)
	}
	dir := filepath.Join(m.config.StoreRoot, "pending", request.TransactionID)
	var pending pendingRecord
	if err := readJSON(filepath.Join(dir, "pending.json"), &pending); err != nil || pending.TransactionID != request.TransactionID || filepath.Clean(pending.StagingPath) != filepath.Join(dir, "data") {
		return fail("transaction-not-found", errors.Join(err, errors.New("transaction identity mismatch")))
	}
	parentID := parentID(m.backend.Provider(), pending.CompatibilityKey)
	parentDir := filepath.Join(m.config.StoreRoot, "parents", parentID)
	if _, err := os.Lstat(parentDir); err == nil {
		existing, readErr := m.readParent(parentID)
		if readErr == nil && existing.Compatibility == pending.CompatibilityKey {
			if verifyErr := verifyParent(existing); verifyErr == nil {
				if receiptErr := m.writeParentReceipt(request.TransactionID, parentID); receiptErr != nil {
					return fail("parent-commit-failed", receiptErr)
				}
				cleanupPendingTransaction(dir)
				return v2.Response{SchemaVersion: 2, RequestID: request.RequestID, OK: true, Provider: m.backend.Provider(), Parent: &existing.Parent}
			}
		}
		if quarantineErr := m.quarantineParentID(parentID); quarantineErr != nil {
			return fail("parent-corrupt", errors.Join(readErr, quarantineErr))
		}
	} else if !os.IsNotExist(err) {
		return fail("parent-commit-failed", err)
	}

	publication := filepath.Join(dir, "publication")
	publicationData := filepath.Join(publication, "data")
	if _, err := os.Lstat(pending.StagingPath); err == nil {
		if err := os.Mkdir(publication, 0700); err != nil && !os.IsExist(err) {
			return fail("parent-commit-failed", err)
		}
		if _, err := os.Lstat(publicationData); err == nil {
			return fail("parent-commit-failed", errors.New("both staging and publication data exist"))
		} else if !os.IsNotExist(err) {
			return fail("parent-commit-failed", err)
		}
		if err := os.Rename(pending.StagingPath, publicationData); err != nil {
			return fail("parent-commit-failed", err)
		}
	} else if !os.IsNotExist(err) {
		return fail("parent-commit-failed", err)
	} else if _, err := os.Lstat(publicationData); err != nil {
		return fail("transaction-not-found", errors.Join(err, errors.New("transaction has no resumable publication data")))
	}

	digest, logical, err := digestTree(publicationData)
	if err != nil {
		return fail("parent-invalid", err)
	}
	usage, err := fileusage.MeasureDirectoryUsage(publicationData)
	if err != nil {
		return fail("parent-invalid", err)
	}
	device, inode, err := directoryIdentity(publicationData)
	if err != nil {
		return fail("parent-commit-failed", err)
	}
	parent := parentRecord{Parent: v2.Parent{ParentID: parentID, Compatibility: pending.CompatibilityKey, Provider: m.backend.Provider(), ArtifactKind: "directory", ContentDigest: digest, LogicalBytes: logical, AllocatedBytes: usage.AllocatedBytes, Immutable: true, CreatedAt: time.Now().UTC()}, DataPath: filepath.Join(parentDir, "data"), Device: device, Inode: inode}
	if err := writeJSON(filepath.Join(publication, "metadata.json"), parent); err != nil {
		return fail("parent-commit-failed", err)
	}
	candidate := parent
	candidate.DataPath = publicationData
	if err := verifyParent(candidate); err != nil {
		return fail("parent-invalid", err)
	}
	if err := os.Rename(publication, parentDir); err != nil {
		return fail("parent-commit-failed", err)
	}
	if err := m.writeParentReceipt(request.TransactionID, parentID); err != nil {
		return fail("parent-commit-failed", err)
	}
	cleanupPendingTransaction(dir)
	return v2.Response{SchemaVersion: 2, RequestID: request.RequestID, OK: true, Provider: m.backend.Provider(), Parent: &parent.Parent}
}

func (m *Manager) parentAbort(request v2.Request, fail failFunc) v2.Response {
	if !identifier.MatchString(request.TransactionID) {
		return fail("invalid-transaction", errors.New("invalid transactionId"))
	}
	dir := filepath.Join(m.config.StoreRoot, "pending", request.TransactionID)
	var pending pendingRecord
	if err := readJSON(filepath.Join(dir, "pending.json"), &pending); err != nil || pending.TransactionID != request.TransactionID || !pathWithin(filepath.Join(m.config.StoreRoot, "pending"), dir) {
		return fail("transaction-not-found", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fail("parent-abort-failed", err)
	}
	return v2.Response{SchemaVersion: 2, RequestID: request.RequestID, OK: true, Provider: m.backend.Provider()}
}

func (m *Manager) acquire(ctx context.Context, request v2.Request, fail failFunc) v2.Response {
	if !identifier.MatchString(request.ConsumerID) || !identifier.MatchString(request.WorkspaceID) || !identifier.MatchString(request.ParentID) {
		return fail("invalid-request", errors.New("invalid consumerId, workspaceId, or parentId"))
	}
	parent, err := m.readParent(request.ParentID)
	if err != nil {
		return fail("parent-not-found", err)
	}
	if err := verifyParent(parent); err != nil {
		m.manualRecovery = true
		_ = m.quarantineParent(parent)
		return fail("parent-corrupt", err)
	}
	status := m.measureStatus()
	reserve := m.config.ChildReserveBytes
	quota := m.config.QuotaBytes
	if request.Limits.StoreMaxAllocatedBytes > 0 && request.Limits.StoreMaxAllocatedBytes < quota {
		quota = request.Limits.StoreMaxAllocatedBytes
	}
	floor := m.config.HostFloorBytes
	if request.Limits.MinimumHostFreeBytes > floor {
		floor = request.Limits.MinimumHostFreeBytes
	}
	if status.AllocatedBytes+reserve > quota || status.HostFreeBytes-reserve < floor {
		return fail("storage-capacity-unavailable", errors.New("quota or host free-space floor would be exceeded"))
	}
	workspacePath := filepath.Join(m.config.WorkspaceRoot, request.WorkspaceID)
	mountPath := filepath.Join(workspacePath, "Library")
	if err := validateWorkspace(m.config.WorkspaceRoot, workspacePath, mountPath); err != nil {
		return fail("invalid-workspace", err)
	}
	leaseID, _ := randomID("lease")
	ownerToken, _ := randomID("owner")
	childPath := filepath.Join(m.config.StoreRoot, "children", leaseID)
	owner := workspaceOwner{SchemaVersion: 2, LeaseID: leaseID, WorkspaceID: request.WorkspaceID, WorkspacePath: workspacePath, MountPath: mountPath, OwnerToken: ownerToken}
	if err := writeJSONExclusive(filepath.Join(workspacePath, workspaceOwnerFile), owner); err != nil {
		return fail("workspace-owner-write-failed", err)
	}
	journal := leaseJournal{Lease: v2.Lease{LeaseID: leaseID, ConsumerID: request.ConsumerID, WorkspaceID: request.WorkspaceID, ParentID: request.ParentID, WorkspacePath: workspacePath, MountPath: mountPath, State: "requested", CreatedAt: time.Now().UTC()}, ChildPath: childPath, ClientPID: request.ClientPID, BootSessionID: m.bootID, OwnerToken: ownerToken, UpdatedAt: time.Now().UTC()}
	journalPath := filepath.Join(m.config.StoreRoot, "leases", leaseID+".json")
	if err := writeJSON(journalPath, journal); err != nil {
		return fail("journal-write-failed", err)
	}
	started := time.Now()
	lease, raw, err := m.backend.Acquire(ctx, storage.AcquireRequest{ParentPath: parent.DataPath, ChildPath: childPath, MountPath: mountPath, StoreRoot: filepath.Join(m.config.StoreRoot, "children"), LeaseID: leaseID}, nil)
	if err != nil {
		_ = os.Remove(filepath.Join(workspacePath, workspaceOwnerFile))
		if _, statErr := os.Lstat(childPath); os.IsNotExist(statErr) {
			_ = os.Remove(journalPath)
		}
		return fail(storageErrorCode(err), err)
	}
	snapshot, err := m.snapshotLease(lease)
	if err != nil {
		return fail("journal-write-failed", err)
	}
	journal.State = "ready"
	journal.Snapshot = &snapshot
	journal.UpdatedAt = time.Now().UTC()
	if err := writeJSON(journalPath, journal); err != nil {
		_, _ = lease.Release(context.Background(), true, nil)
		return fail("journal-write-failed", err)
	}
	m.leases[leaseID] = lease
	metrics := metricsFromStorage(raw)
	metrics.AcquireWallClockMs = time.Since(started).Milliseconds()
	return v2.Response{SchemaVersion: 2, RequestID: request.RequestID, OK: true, Provider: m.backend.Provider(), Parent: &parent.Parent, Lease: &journal.Lease, Metrics: &metrics}
}

func (m *Manager) release(ctx context.Context, request v2.Request, fail failFunc) v2.Response {
	if !identifier.MatchString(request.LeaseID) {
		return fail("invalid-lease", errors.New("invalid leaseId"))
	}
	journal, err := m.readLease(request.LeaseID)
	if err != nil {
		return fail("lease-not-found", err)
	}
	lease := m.leases[request.LeaseID]
	if lease == nil && journal.Snapshot != nil {
		lease, err = m.recoverLease(*journal.Snapshot)
		if err != nil {
			m.manualRecovery = true
			return fail("lease-recovery-failed", err)
		}
	}
	if lease == nil {
		return fail("lease-not-active", errors.New("lease has no recoverable ownership snapshot"))
	}
	if err := validateWorkspaceOwner(journal); err != nil {
		m.manualRecovery = true
		return fail("workspace-ownership-mismatch", err)
	}
	started := time.Now()
	raw, err := lease.Release(ctx, true, nil)
	if err != nil {
		m.manualRecovery = true
		return fail(storageErrorCode(err), err)
	}
	if err := removeWorkspaceOwner(journal); err != nil {
		return fail("workspace-cleanup-failed", err)
	}
	_ = os.Remove(filepath.Join(m.config.StoreRoot, "leases", request.LeaseID+".json"))
	delete(m.leases, request.LeaseID)
	journal.State = "released"
	metrics := metricsFromStorage(raw)
	metrics.ReleaseWallClockMs = time.Since(started).Milliseconds()
	return v2.Response{SchemaVersion: 2, RequestID: request.RequestID, OK: true, Provider: m.backend.Provider(), Lease: &journal.Lease, Metrics: &metrics}
}

func (m *Manager) status(requestID string) v2.Response {
	s := m.measureStatus()
	return v2.Response{SchemaVersion: 2, RequestID: requestID, OK: true, Provider: m.backend.Provider(), Status: &s}
}

func (m *Manager) measureStatus() v2.Status {
	usage, _ := fileusage.MeasureDirectoryUsage(m.config.StoreRoot)
	free, _ := hostFreeBytes(m.config.StoreRoot)
	parents, _ := os.ReadDir(filepath.Join(m.config.StoreRoot, "parents"))
	leases, _ := os.ReadDir(filepath.Join(m.config.StoreRoot, "leases"))
	quarantine, _ := os.ReadDir(filepath.Join(m.config.StoreRoot, "quarantine"))
	children, _ := os.ReadDir(filepath.Join(m.config.StoreRoot, "children"))
	quarantineCount := len(quarantine)
	for _, entry := range children {
		if strings.HasPrefix(entry.Name(), ".testplay-delete-") {
			quarantineCount++
		}
	}
	return v2.Status{Capability: m.capability, ParentCount: len(parents), ActiveLeaseCount: len(leases), QuarantineCount: quarantineCount, AllocatedBytes: usage.AllocatedBytes, QuotaBytes: m.config.QuotaBytes, HostFreeBytes: free, HostFloorBytes: m.config.HostFloorBytes, ManualRecoveryRequired: m.manualRecovery}
}

func (m *Manager) probe(ctx context.Context) error {
	root := filepath.Join(m.config.StoreRoot, ".cow-probe")
	_ = os.RemoveAll(root)
	defer os.RemoveAll(root)
	parent := filepath.Join(root, "parent")
	child := filepath.Join(root, "children", "probe-child")
	mount := filepath.Join(root, "mount")
	if err := os.MkdirAll(parent, 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(child), 0700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(parent, "probe"), []byte("cow-probe"), 0600); err != nil {
		return err
	}
	lease, _, err := m.backend.Acquire(ctx, storage.AcquireRequest{ParentPath: parent, ChildPath: child, MountPath: mount, StoreRoot: filepath.Join(root, "children"), LeaseID: "probe-lease"}, nil)
	if err != nil {
		return err
	}
	_, err = lease.Release(ctx, true, nil)
	return err
}

func (m *Manager) recover(ctx context.Context) error {
	entries, err := os.ReadDir(filepath.Join(m.config.StoreRoot, "leases"))
	if err != nil {
		return err
	}
	var result error
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		var journal leaseJournal
		if err := readJSON(filepath.Join(m.config.StoreRoot, "leases", entry.Name()), &journal); err != nil {
			result = errors.Join(result, err)
			continue
		}
		if journal.Snapshot == nil {
			if _, err := os.Lstat(journal.ChildPath); os.IsNotExist(err) {
				if ownerErr := removeWorkspaceOwner(&journal); ownerErr != nil && !os.IsNotExist(ownerErr) {
					result = errors.Join(result, ownerErr)
					continue
				}
				_ = os.Remove(filepath.Join(m.config.StoreRoot, "leases", entry.Name()))
				continue
			}
			result = errors.Join(result, errors.New("partial lease lacks ownership snapshot"))
			continue
		}
		lease, err := m.recoverLease(*journal.Snapshot)
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		alive := journal.BootSessionID == m.bootID && processAlive(journal.ClientPID)
		if alive {
			m.leases[journal.LeaseID] = lease
			continue
		}
		if err := validateWorkspaceOwner(&journal); err != nil {
			result = errors.Join(result, err)
			continue
		}
		if _, err := lease.Release(ctx, true, nil); err != nil {
			result = errors.Join(result, err)
			continue
		}
		if err := removeWorkspaceOwner(&journal); err != nil {
			result = errors.Join(result, err)
			continue
		}
		_ = os.Remove(filepath.Join(m.config.StoreRoot, "leases", entry.Name()))
	}
	return result
}

func (m *Manager) readParent(id string) (parentRecord, error) {
	var p parentRecord
	err := readJSON(filepath.Join(m.config.StoreRoot, "parents", id, "metadata.json"), &p)
	if err != nil {
		return p, err
	}
	if p.ParentID != id || p.Provider != m.backend.Provider() || filepath.Clean(p.DataPath) != filepath.Join(m.config.StoreRoot, "parents", id, "data") {
		return p, errors.New("parent identity mismatch")
	}
	return p, nil
}
func (m *Manager) readParentByCompatibility(key string) (parentRecord, error) {
	id := parentID(m.backend.Provider(), key)
	return m.readParent(id)
}

func (m *Manager) quarantineParent(parent parentRecord) error {
	if filepath.Clean(filepath.Dir(parent.DataPath)) != filepath.Join(m.config.StoreRoot, "parents", parent.ParentID) {
		return errors.New("refusing to quarantine parent outside owned path")
	}
	return m.quarantineParentID(parent.ParentID)
}
func (m *Manager) quarantineParentID(parentID string) error {
	if !identifier.MatchString(parentID) {
		return errors.New("invalid parent quarantine identity")
	}
	parentDir := filepath.Join(m.config.StoreRoot, "parents", parentID)
	destination := filepath.Join(m.config.StoreRoot, "quarantine", parentID+fmt.Sprintf("-%d", time.Now().UnixNano()))
	return os.Rename(parentDir, destination)
}
func (m *Manager) readLease(id string) (*leaseJournal, error) {
	var j leaseJournal
	err := readJSON(filepath.Join(m.config.StoreRoot, "leases", id+".json"), &j)
	if err != nil {
		return nil, err
	}
	if j.LeaseID != id {
		return nil, errors.New("lease identity mismatch")
	}
	return &j, nil
}
func (m *Manager) readParentReceipt(transactionID string) (parentReceipt, error) {
	var receipt parentReceipt
	err := readJSON(filepath.Join(m.config.StoreRoot, "receipts", transactionID+".json"), &receipt)
	if err != nil {
		return receipt, err
	}
	if receipt.TransactionID != transactionID || !identifier.MatchString(receipt.ParentID) {
		return receipt, errors.New("parent receipt identity mismatch")
	}
	return receipt, nil
}
func (m *Manager) writeParentReceipt(transactionID, parentID string) error {
	return writeJSON(filepath.Join(m.config.StoreRoot, "receipts", transactionID+".json"), parentReceipt{TransactionID: transactionID, ParentID: parentID})
}

func verifyParent(parent parentRecord) error {
	device, inode, err := directoryIdentity(parent.DataPath)
	if err != nil {
		return err
	}
	if device != parent.Device || inode != parent.Inode {
		return errors.New("committed parent identity changed")
	}
	digest, logical, err := digestTree(parent.DataPath)
	if err != nil {
		return err
	}
	if digest != parent.ContentDigest || logical != parent.LogicalBytes || !parent.Immutable {
		return errors.New("committed parent content changed")
	}
	return nil
}

func directoryIdentity(path string) (uint64, uint64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, 0, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return 0, 0, errors.New("parent data path is not a real directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, errors.New("parent filesystem identity is unavailable")
	}
	return uint64(stat.Dev), uint64(stat.Ino), nil
}
func validateWorkspace(root, path, mount string) error {
	if !pathWithin(root, path) {
		return errors.New("workspace escapes configured root")
	}
	for _, p := range []string{root, path} {
		info, err := os.Lstat(p)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("workspace component must be a real directory: %s", p)
		}
	}
	if _, err := os.Lstat(mount); err == nil {
		return errors.New("Library mount already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}
func validateWorkspaceOwner(j *leaseJournal) error {
	var owner workspaceOwner
	if err := readJSON(filepath.Join(j.WorkspacePath, workspaceOwnerFile), &owner); err != nil {
		return err
	}
	if owner.SchemaVersion != 2 || owner.LeaseID != j.LeaseID || owner.WorkspaceID != j.WorkspaceID || filepath.Clean(owner.WorkspacePath) != filepath.Clean(j.WorkspacePath) || filepath.Clean(owner.MountPath) != filepath.Clean(j.MountPath) || owner.OwnerToken != j.OwnerToken {
		return errors.New("workspace owner mismatch")
	}
	return nil
}
func removeWorkspaceOwner(j *leaseJournal) error {
	if err := validateWorkspaceOwner(j); err != nil {
		return err
	}
	return os.Remove(filepath.Join(j.WorkspacePath, workspaceOwnerFile))
}
func parentID(provider, key string) string {
	sum := sha256.Sum256([]byte(provider + "\x00" + key))
	return "parent-" + hex.EncodeToString(sum[:16])
}
func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}
func randomID(prefix string) (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return prefix + "-" + hex.EncodeToString(value), nil
}
func digestTree(root string) (string, int64, error) {
	type treeEntry struct {
		relative  string
		mode      fs.FileMode
		directory bool
	}
	var entries []treeEntry
	err := filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink is not allowed: %s", path)
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		if !e.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("special file is not allowed: %s", path)
		}
		if path != root && (e.Name() == reservedStorageMarker || e.Name() == workspaceOwnerFile) {
			return fmt.Errorf("reserved marker is not allowed: %s", path)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		entries = append(entries, treeEntry{relative: filepath.ToSlash(rel), mode: info.Mode().Perm(), directory: e.IsDir()})
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].relative < entries[j].relative })
	h := sha256.New()
	_, _ = h.Write([]byte("unity-workspace-storage-tree-v2\x00"))
	var logical int64
	for _, entry := range entries {
		kind := byte('f')
		if entry.directory {
			kind = 'd'
		}
		_, _ = h.Write([]byte{kind})
		writeDigestField(h, []byte(entry.relative))
		var mode [4]byte
		binary.BigEndian.PutUint32(mode[:], uint32(entry.mode.Perm()))
		_, _ = h.Write(mode[:])
		if entry.directory {
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(entry.relative))
		f, err := os.Open(path)
		if err != nil {
			return "", 0, err
		}
		info, _ := f.Stat()
		logical += info.Size()
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(info.Size()))
		_, _ = h.Write(size[:])
		_, copyErr := io.Copy(h, f)
		closeErr := f.Close()
		if copyErr != nil || closeErr != nil {
			return "", 0, errors.Join(copyErr, closeErr)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), logical, nil
}
func writeDigestField(writer io.Writer, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = writer.Write(size[:])
	_, _ = writer.Write(value)
}
func cleanupPendingTransaction(dir string) {
	_ = os.Remove(filepath.Join(dir, "pending.json"))
	_ = os.Remove(dir)
}
func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.WriteDurable(path, append(data, '\n'), 0600)
}
func writeJSONExclusive(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.WriteExclusiveDurable(path, append(data, '\n'), 0600)
}
func readJSON(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}
func storageErrorCode(err error) string {
	var value *storage.Error
	if errors.As(err, &value) && value.Code != "" {
		return value.Code
	}
	return "storage-operation-failed"
}
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
func metricsFromStorage(raw storage.Metrics) v2.Metrics {
	m := v2.Metrics{}
	if raw.ChildCreateMs != nil {
		m.ChildCreateMs = *raw.ChildCreateMs
	}
	if raw.ChildReadyLogicalBytes != nil {
		m.ChildReadyLogicalBytes = *raw.ChildReadyLogicalBytes
	}
	if raw.ChildReadyAllocatedBytes != nil {
		m.ChildReadyAllocatedBytes = *raw.ChildReadyAllocatedBytes
	}
	if raw.CleanupMs != nil {
		m.CleanupMs = *raw.CleanupMs
	}
	return m
}
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
