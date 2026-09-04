package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

const (
	removalPrepared        = "prepared"
	removalStorageReleased = "storage-released"
	removalCommitted       = "committed"
	removalAborted         = "aborted"
)

type retainedRemoval struct {
	receipt     RemovalReceipt
	reservation ChildRemoval
	expiresAt   time.Time
	timer       *time.Timer
	committing  bool
}

func (b *Broker) prepareRetainedRemoval(ctx context.Context, request Request, fail failureBuilder) Response {
	if !identifierPattern.MatchString(request.RunID) || !identifierPattern.MatchString(request.TransactionID) || !identifierPattern.MatchString(request.WorkspaceID) {
		return fail("invalid-request", "prepare-retained-removal", request.RunID, ErrInvalidInput)
	}
	receipt, receiptErr := b.store.ReadRemovalReceipt(request.RunID)
	if receiptErr != nil && !os.IsNotExist(receiptErr) {
		return fail("removal-receipt-invalid", "prepare-retained-removal", request.RunID, receiptErr)
	}
	if receipt != nil && receipt.TransactionID != request.TransactionID {
		return fail("removal-transaction-conflict", "prepare-retained-removal", request.RunID, ErrParentConflict)
	}
	if receipt != nil && receipt.State == removalCommitted {
		return b.removalResponse(request, *receipt, removalCommitted, time.Time{})
	}

	b.mu.Lock()
	activeRemoval := b.removals[request.RunID]
	b.mu.Unlock()
	if activeRemoval != nil {
		if activeRemoval.receipt.TransactionID != request.TransactionID {
			return fail("removal-transaction-conflict", "prepare-retained-removal", request.RunID, ErrParentConflict)
		}
		return b.removalResponse(request, activeRemoval.receipt, removalPrepared, activeRemoval.expiresAt)
	}

	record, journal, response := b.readRetainedRemovalState(request, fail)
	if response.Error != nil {
		return response
	}
	if receipt != nil && (receipt.LeaseID != record.LeaseID || receipt.OwnershipToken != record.OwnershipToken || !samePath(receipt.ChildPath, record.ChildPath)) {
		return fail("removal-receipt-invalid", "prepare-retained-removal", request.RunID, ErrOwnershipMismatch)
	}
	if _, statErr := os.Lstat(record.ChildPath); os.IsNotExist(statErr) {
		if receipt == nil {
			return fail("retained-identity-mismatch", "prepare-retained-removal", record.ChildPath, ErrOwnershipMismatch)
		}
		return b.removalResponse(request, *receipt, removalPrepared, time.Time{})
	} else if statErr != nil {
		return fail("retained-identity-mismatch", "prepare-retained-removal", record.ChildPath, statErr)
	}

	session, sessionResponse := b.retainedSession(ctx, request, *record, *journal, fail)
	if sessionResponse.Error != nil {
		return sessionResponse
	}
	reservation, err := session.PrepareRemoval(ctx)
	if err != nil {
		if errors.Is(err, ErrVolumeInUse) {
			return fail("retained-in-use", "prepare-retained-removal", journal.MountPath, err)
		}
		return fail("retained-removal-prepare-failed", "prepare-retained-removal", journal.MountPath, err)
	}
	if receipt == nil {
		receipt = &RemovalReceipt{TransactionID: request.TransactionID, RunID: record.RunID, LeaseID: record.LeaseID, OwnershipToken: record.OwnershipToken, ChildPath: record.ChildPath, State: removalPrepared}
	}
	receipt.State = removalPrepared
	if err := b.store.WriteRemovalReceipt(*receipt); err != nil {
		abortErr := reservation.Abort()
		return fail("removal-receipt-write-failed", "prepare-retained-removal", record.ChildPath, errors.Join(err, abortErr))
	}
	expiresAt := b.now().UTC().Add(b.config.RemovalTTL)
	entry := &retainedRemoval{receipt: *receipt, reservation: reservation, expiresAt: expiresAt}
	entry.timer = time.AfterFunc(b.config.RemovalTTL, func() { b.expireRetainedRemoval(request.RunID, request.TransactionID) })
	b.mu.Lock()
	if conflict := b.removals[request.RunID]; conflict != nil {
		b.mu.Unlock()
		entry.timer.Stop()
		_ = reservation.Abort()
		return fail("removal-transaction-conflict", "prepare-retained-removal", request.RunID, ErrParentConflict)
	}
	b.removals[request.RunID] = entry
	b.mu.Unlock()
	return b.removalResponse(request, *receipt, removalPrepared, expiresAt)
}

func (b *Broker) commitRetainedRemoval(ctx context.Context, request Request, fail failureBuilder) Response {
	if !identifierPattern.MatchString(request.RunID) || !identifierPattern.MatchString(request.TransactionID) {
		return fail("invalid-request", "commit-retained-removal", request.RunID, ErrInvalidInput)
	}
	receipt, err := b.store.ReadRemovalReceipt(request.RunID)
	if err != nil {
		return fail("removal-not-prepared", "commit-retained-removal", request.RunID, err)
	}
	if receipt.TransactionID != request.TransactionID {
		return fail("removal-transaction-conflict", "commit-retained-removal", request.RunID, ErrParentConflict)
	}
	if receipt.State == removalCommitted {
		return b.removalResponse(request, *receipt, removalCommitted, time.Time{})
	}
	b.mu.Lock()
	entry := b.removals[request.RunID]
	if entry != nil && entry.receipt.TransactionID == request.TransactionID {
		entry.committing = true
		if entry.timer != nil {
			entry.timer.Stop()
		}
	}
	b.mu.Unlock()
	if entry == nil {
		if _, statErr := os.Lstat(receipt.ChildPath); statErr == nil {
			return fail("removal-not-prepared", "commit-retained-removal", request.RunID, fmt.Errorf("prepare must be repeated after broker restart"))
		} else if !os.IsNotExist(statErr) {
			return fail("retained-identity-mismatch", "commit-retained-removal", receipt.ChildPath, statErr)
		}
	} else {
		metrics, commitErr := entry.reservation.Commit(ctx)
		if commitErr != nil {
			return fail("retained-removal-commit-failed", "commit-retained-removal", receipt.ChildPath, commitErr)
		}
		receipt.State = removalStorageReleased
		if err := b.store.WriteRemovalReceipt(*receipt); err != nil {
			return fail("removal-receipt-write-failed", "commit-retained-removal", receipt.ChildPath, err)
		}
		_ = metrics
	}
	if err := b.finalizeRetainedRemoval(*receipt); err != nil {
		return fail("retained-removal-finalize-failed", "commit-retained-removal", receipt.ChildPath, err)
	}
	receipt.State = removalCommitted
	if err := b.store.WriteRemovalReceipt(*receipt); err != nil {
		return fail("removal-receipt-write-failed", "commit-retained-removal", receipt.ChildPath, err)
	}
	b.mu.Lock()
	delete(b.removals, request.RunID)
	delete(b.children, receipt.LeaseID)
	b.mu.Unlock()
	return b.removalResponse(request, *receipt, removalCommitted, time.Time{})
}

func (b *Broker) abortRetainedRemoval(request Request, fail failureBuilder) Response {
	if !identifierPattern.MatchString(request.RunID) || !identifierPattern.MatchString(request.TransactionID) {
		return fail("invalid-request", "abort-retained-removal", request.RunID, ErrInvalidInput)
	}
	receipt, err := b.store.ReadRemovalReceipt(request.RunID)
	if os.IsNotExist(err) {
		return Response{SchemaVersion: ProtocolSchemaVersion, RequestID: request.RequestID, OK: true, Provider: Provider, Removal: &Removal{TransactionID: request.TransactionID, RunID: request.RunID, State: removalAborted}}
	}
	if err != nil {
		return fail("removal-receipt-invalid", "abort-retained-removal", request.RunID, err)
	}
	if receipt.TransactionID != request.TransactionID {
		return fail("removal-transaction-conflict", "abort-retained-removal", request.RunID, ErrParentConflict)
	}
	if receipt.State == removalCommitted || receipt.State == removalStorageReleased {
		return fail("removal-already-committed", "abort-retained-removal", request.RunID, ErrOwnershipMismatch)
	}
	b.mu.Lock()
	entry := b.removals[request.RunID]
	if entry != nil && entry.committing {
		b.mu.Unlock()
		return fail("removal-already-committing", "abort-retained-removal", request.RunID, ErrParentConflict)
	}
	delete(b.removals, request.RunID)
	b.mu.Unlock()
	if entry != nil {
		if entry.timer != nil {
			entry.timer.Stop()
		}
		if err := entry.reservation.Abort(); err != nil {
			return fail("retained-removal-abort-failed", "abort-retained-removal", receipt.ChildPath, err)
		}
	}
	if err := b.store.RemoveRemovalReceipt(*receipt); err != nil && !os.IsNotExist(err) {
		return fail("removal-receipt-remove-failed", "abort-retained-removal", receipt.ChildPath, err)
	}
	return b.removalResponse(request, *receipt, removalAborted, time.Time{})
}

func (b *Broker) readRetainedRemovalState(request Request, fail failureBuilder) (*RetainedRecord, *LeaseJournal, Response) {
	record, err := b.store.ReadRetained(request.RunID)
	if err != nil {
		return nil, nil, fail("retained-not-found", "prepare-retained-removal", request.RunID, err)
	}
	journal, err := b.store.ReadLease(record.LeaseID)
	if err != nil || !journal.Retained || journal.OwnershipToken != record.OwnershipToken || !samePath(journal.ChildPath, record.ChildPath) {
		return nil, nil, fail("retained-identity-mismatch", "prepare-retained-removal", record.ChildPath, errors.Join(err, ErrOwnershipMismatch))
	}
	return record, journal, Response{}
}

func (b *Broker) retainedSession(ctx context.Context, request Request, record RetainedRecord, journal LeaseJournal, fail failureBuilder) (ChildSession, Response) {
	b.mu.Lock()
	session := b.children[record.LeaseID]
	b.mu.Unlock()
	if session != nil {
		return session, Response{}
	}
	resolved, err := b.store.ResolveParent(CompatibilityKey{SchemaVersion: ParentSchemaVersion, Digest: record.ParentKey})
	if err != nil || resolved.Status != ParentStatusValid || resolved.Metadata == nil {
		return nil, fail("parent-unavailable", "prepare-retained-removal", record.ParentKey, errors.Join(err, ErrOwnershipMismatch))
	}
	if err := b.native.VerifyParent(ctx, *resolved.Metadata); err != nil {
		return nil, fail("parent-corrupt", "prepare-retained-removal", record.ParentKey, err)
	}
	_, mount, err := b.workspaceMount(request.WorkspaceID)
	if err != nil {
		return nil, fail("invalid-workspace", "prepare-retained-removal", request.WorkspaceID, err)
	}
	if err := b.validateWorkspaceContainer(request.WorkspaceID, mount); err != nil {
		return nil, fail("invalid-workspace", "validate-retained-mount", mount, err)
	}
	journal.MountPath = mount
	session, _, err = b.native.AttachChild(ctx, *resolved.Metadata, journal)
	if err != nil {
		return nil, fail("retained-attach-failed", "prepare-retained-removal", record.ChildPath, err)
	}
	b.mu.Lock()
	b.children[record.LeaseID] = session
	b.mu.Unlock()
	return session, Response{}
}

func (b *Broker) finalizeRetainedRemoval(receipt RemovalReceipt) error {
	journal, journalErr := b.store.ReadLease(receipt.LeaseID)
	if journalErr == nil {
		if journal.OwnershipToken != receipt.OwnershipToken || !samePath(journal.ChildPath, receipt.ChildPath) {
			return ErrOwnershipMismatch
		}
		if err := b.cleanupOwnedWorkspace(*journal); err != nil {
			return err
		}
		if err := b.store.RemoveLease(*journal); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else if !os.IsNotExist(journalErr) {
		return journalErr
	}
	record, recordErr := b.store.ReadRetained(receipt.RunID)
	if recordErr == nil {
		if record.LeaseID != receipt.LeaseID || record.OwnershipToken != receipt.OwnershipToken || !samePath(record.ChildPath, receipt.ChildPath) {
			return ErrOwnershipMismatch
		}
		if err := b.store.RemoveRetained(*record); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else if !os.IsNotExist(recordErr) {
		return recordErr
	}
	return nil
}

func (b *Broker) expireRetainedRemoval(runID, transactionID string) {
	b.mu.Lock()
	entry := b.removals[runID]
	if entry == nil || entry.receipt.TransactionID != transactionID || entry.committing {
		b.mu.Unlock()
		return
	}
	delete(b.removals, runID)
	b.mu.Unlock()
	_ = entry.reservation.Abort()
	receipt, err := b.store.ReadRemovalReceipt(runID)
	if err == nil && receipt.TransactionID == transactionID && receipt.State == removalPrepared {
		_ = b.store.RemoveRemovalReceipt(*receipt)
	}
}

func (b *Broker) removalResponse(request Request, receipt RemovalReceipt, state string, expiresAt time.Time) Response {
	var expiry *time.Time
	if !expiresAt.IsZero() {
		expiry = &expiresAt
	}
	return Response{SchemaVersion: ProtocolSchemaVersion, RequestID: request.RequestID, OK: true, Provider: Provider, Removal: &Removal{TransactionID: receipt.TransactionID, RunID: receipt.RunID, LeaseID: receipt.LeaseID, State: state, ExpiresAt: expiry}}
}
