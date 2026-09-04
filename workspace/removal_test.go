package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func retainTestChild(t *testing.T, broker *Broker, key CompatibilityKey, workspaces, runID string) Lease {
	t.Helper()
	workspace := filepath.Join(workspaces, runID)
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	acquire := request(OperationAcquire, "acquire-"+runID)
	acquire.ParentKey = &key
	acquire.RunID = runID
	acquire.WorkspaceID = runID
	ready := broker.Handle(context.Background(), "S-1-5-21-test", acquire)
	if !ready.OK || ready.Lease == nil {
		t.Fatalf("acquire=%+v", ready)
	}
	release := request(OperationRelease, "retain-"+runID)
	release.LeaseID = ready.Lease.LeaseID
	release.RetainChild = true
	retained := broker.Handle(context.Background(), "S-1-5-21-test", release)
	if !retained.OK {
		t.Fatalf("retain=%+v", retained)
	}
	return *ready.Lease
}

func attachTestRetained(t *testing.T, broker *Broker, runID string) *fakeChildSession {
	t.Helper()
	attach := request(OperationAttachRetained, "attach-"+runID)
	attach.RunID = runID
	attach.WorkspaceID = runID
	response := broker.Handle(context.Background(), "S-1-5-21-test", attach)
	if !response.OK || response.Lease == nil {
		t.Fatalf("attach=%+v", response)
	}
	session, ok := broker.children[response.Lease.LeaseID].(*fakeChildSession)
	if !ok {
		t.Fatalf("child session=%T", broker.children[response.Lease.LeaseID])
	}
	return session
}

func removalRequest(operation, requestID, runID, workspaceID, transactionID string) Request {
	value := request(operation, requestID)
	value.RunID = runID
	value.WorkspaceID = workspaceID
	value.TransactionID = transactionID
	return value
}

func TestBrokerWireSchemaV3IsIndependentFromV2DiskRecords(t *testing.T) {
	broker, _, _ := testBroker(t, &fakeNative{})
	legacy := Request{SchemaVersion: 2, Operation: OperationHello, RequestID: "legacy-schema"}
	response := broker.Handle(context.Background(), "S-1-5-21-test", legacy)
	if response.OK || response.Error == nil || response.Error.Code != "unsupported-schema" {
		t.Fatalf("legacy response=%+v", response)
	}
	hello := broker.Handle(context.Background(), "S-1-5-21-test", request(OperationHello, "hello-v3"))
	if !hello.OK || hello.SchemaVersion != 3 || hello.BrokerVersion != "v3" {
		t.Fatalf("hello=%+v", hello)
	}
	journal := LeaseJournal{LeaseID: "lease-disk-v2", RunID: "disk-v2", OwnershipToken: "owner", ParentKey: strings.Repeat("a", 64), ChildPath: filepath.Join(broker.store.paths.Children, "lease-disk-v2.vhdx")}
	if err := broker.store.WriteLease(journal); err != nil {
		t.Fatal(err)
	}
	path, _ := broker.store.paths.Lease(journal.LeaseID)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"schemaVersion": 2`) {
		t.Fatalf("journal schema changed with wire protocol: %s", raw)
	}
	if _, err := broker.store.ReadLease(journal.LeaseID); err != nil {
		t.Fatalf("v2 journal was not readable: %v", err)
	}
}

func TestAttachRetainedAllowsNativeToReconcileExistingLibraryLeaf(t *testing.T) {
	native := &fakeNative{}
	broker, key, workspaces := testBroker(t, native)
	commitTestParent(t, broker, key, workspaces)
	retainTestChild(t, broker, key, workspaces, "stale-leaf")
	if err := os.Mkdir(filepath.Join(workspaces, "stale-leaf", "Library"), 0700); err != nil {
		t.Fatal(err)
	}
	attachTestRetained(t, broker, "stale-leaf")
	native.mu.Lock()
	attachCalls := native.attachCalls
	native.mu.Unlock()
	if attachCalls != 1 {
		t.Fatalf("native attach calls=%d", attachCalls)
	}
}

func TestBusyRetainedRemovalFailsBeforeManagedStateMutation(t *testing.T) {
	native := &fakeNative{prepareErr: ErrVolumeInUse}
	broker, key, workspaces := testBroker(t, native)
	commitTestParent(t, broker, key, workspaces)
	lease := retainTestChild(t, broker, key, workspaces, "busy-remove")
	attachTestRetained(t, broker, "busy-remove")
	recordBefore, err := broker.store.ReadRetained("busy-remove")
	if err != nil {
		t.Fatal(err)
	}
	journalBefore, err := broker.store.ReadLease(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	prepare := removalRequest(OperationPrepareRetainedRemoval, "prepare-busy", "busy-remove", "busy-remove", "remove-busy")
	response := broker.Handle(context.Background(), "S-1-5-21-test", prepare)
	if response.OK || response.Error == nil || response.Error.Code != "retained-in-use" {
		t.Fatalf("prepare=%+v", response)
	}
	recordAfter, err := broker.store.ReadRetained("busy-remove")
	if err != nil || *recordAfter != *recordBefore {
		t.Fatalf("retained changed: before=%+v after=%+v err=%v", recordBefore, recordAfter, err)
	}
	journalAfter, err := broker.store.ReadLease(lease.LeaseID)
	if err != nil || *journalAfter != *journalBefore {
		t.Fatalf("journal changed: before=%+v after=%+v err=%v", journalBefore, journalAfter, err)
	}
	if _, err := os.Lstat(recordBefore.ChildPath); err != nil {
		t.Fatalf("child changed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(workspaces, "busy-remove", "Library")); err != nil {
		t.Fatalf("Library changed: %v", err)
	}
	if _, err := broker.store.ReadRemovalReceipt("busy-remove"); !os.IsNotExist(err) {
		t.Fatalf("busy preflight left receipt: %v", err)
	}
}

func TestRetainedRemovalTransactionIsIdempotentConflictSafeAndRetryable(t *testing.T) {
	native := &fakeNative{}
	broker, key, workspaces := testBroker(t, native)
	commitTestParent(t, broker, key, workspaces)
	lease := retainTestChild(t, broker, key, workspaces, "transactional-remove")
	session := attachTestRetained(t, broker, "transactional-remove")
	first := removalRequest(OperationPrepareRetainedRemoval, "prepare-first", "transactional-remove", "transactional-remove", "remove-txn-one")
	prepared := broker.Handle(context.Background(), "S-1-5-21-test", first)
	if !prepared.OK || prepared.Removal == nil || prepared.Removal.State != removalPrepared || prepared.Removal.ExpiresAt == nil || prepared.Removal.ExpiresAt.IsZero() {
		t.Fatalf("prepared=%+v", prepared)
	}
	repeated := first
	repeated.RequestID = "prepare-repeat"
	repeatResponse := broker.Handle(context.Background(), "S-1-5-21-test", repeated)
	if !repeatResponse.OK || repeatResponse.Removal == nil || repeatResponse.Removal.TransactionID != first.TransactionID || repeatResponse.Removal.ExpiresAt == nil || !repeatResponse.Removal.ExpiresAt.Equal(*prepared.Removal.ExpiresAt) {
		t.Fatalf("repeated=%+v", repeatResponse)
	}
	conflict := first
	conflict.RequestID = "prepare-conflict"
	conflict.TransactionID = "remove-txn-two"
	conflictResponse := broker.Handle(context.Background(), "S-1-5-21-test", conflict)
	if conflictResponse.OK || conflictResponse.Error == nil || conflictResponse.Error.Code != "removal-transaction-conflict" {
		t.Fatalf("conflict=%+v", conflictResponse)
	}
	commit := removalRequest(OperationCommitRetainedRemoval, "commit-first", "transactional-remove", "", first.TransactionID)
	committed := broker.Handle(context.Background(), "S-1-5-21-test", commit)
	if !committed.OK || committed.Removal == nil || committed.Removal.State != removalCommitted || session.commitCalls != 1 {
		t.Fatalf("committed=%+v commits=%d", committed, session.commitCalls)
	}
	restarted, err := NewBroker(broker.config, native)
	if err != nil {
		t.Fatal(err)
	}
	retry := commit
	retry.RequestID = "commit-response-loss-retry"
	retried := restarted.Handle(context.Background(), "S-1-5-21-test", retry)
	if !retried.OK || retried.Removal == nil || retried.Removal.State != removalCommitted {
		t.Fatalf("retried=%+v", retried)
	}
	if _, err := os.Lstat(filepath.Join(workspaces, "transactional-remove")); !os.IsNotExist(err) {
		t.Fatalf("empty managed workspace remains: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(broker.store.paths.Children, lease.LeaseID+".vhdx")); !os.IsNotExist(err) {
		t.Fatalf("child remains: %v", err)
	}
}

func TestRetainedRemovalAbortAndExpiryReleaseReservation(t *testing.T) {
	native := &fakeNative{}
	broker, key, workspaces := testBroker(t, native)
	broker.config.RemovalTTL = 20 * time.Millisecond
	commitTestParent(t, broker, key, workspaces)
	retainTestChild(t, broker, key, workspaces, "abort-remove")
	session := attachTestRetained(t, broker, "abort-remove")
	prepare := removalRequest(OperationPrepareRetainedRemoval, "prepare-abort", "abort-remove", "abort-remove", "remove-abort")
	if response := broker.Handle(context.Background(), "S-1-5-21-test", prepare); !response.OK {
		t.Fatalf("prepare=%+v", response)
	}
	abort := removalRequest(OperationAbortRetainedRemoval, "abort", "abort-remove", "", prepare.TransactionID)
	if response := broker.Handle(context.Background(), "S-1-5-21-test", abort); !response.OK || response.Removal == nil || response.Removal.State != removalAborted {
		t.Fatalf("abort=%+v", response)
	}
	if session.abortCalls != 1 {
		t.Fatalf("abort calls=%d", session.abortCalls)
	}
	if _, err := broker.store.ReadRemovalReceipt("abort-remove"); !os.IsNotExist(err) {
		t.Fatalf("abort receipt remains: %v", err)
	}
	second := prepare
	second.RequestID = "prepare-expiry"
	second.TransactionID = "remove-expiry"
	if response := broker.Handle(context.Background(), "S-1-5-21-test", second); !response.OK {
		t.Fatalf("second prepare=%+v", response)
	}
	deadline := time.Now().Add(time.Second)
	for {
		_, err := broker.store.ReadRemovalReceipt("abort-remove")
		if os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reservation did not expire: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if session.abortCalls != 2 {
		t.Fatalf("expiry abort calls=%d", session.abortCalls)
	}
	if _, err := os.Lstat(filepath.Join(workspaces, "abort-remove", "Library")); err != nil {
		t.Fatalf("expiry mutated Library: %v", err)
	}
}
