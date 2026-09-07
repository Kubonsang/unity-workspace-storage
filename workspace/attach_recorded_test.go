package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func retainedFixture(t *testing.T) (*Broker, *fakeNative, Request, Request) {
	t.Helper()
	native := &fakeNative{}
	broker, key, workspaces := testBroker(t, native)
	commitTestParent(t, broker, key, workspaces)
	if err := os.Mkdir(filepath.Join(workspaces, "repeat-repair"), 0700); err != nil {
		t.Fatal(err)
	}
	acquire := request(OperationAcquire, "repeat-acquire")
	acquire.ParentKey, acquire.RunID, acquire.WorkspaceID = &key, "repeat-repair", "repeat-repair"
	ready := broker.Handle(context.Background(), "S-1-5-21-test", acquire)
	if !ready.OK {
		t.Fatalf("acquire: %+v", ready)
	}
	release := request(OperationRelease, "repeat-release-0")
	release.LeaseID, release.RetainChild = ready.Lease.LeaseID, true
	if result := broker.Handle(context.Background(), "S-1-5-21-test", release); !result.OK {
		t.Fatalf("retain: %+v", result)
	}
	attach := request(OperationAttachRetained, "repeat-attach-1")
	attach.RunID, attach.WorkspaceID = "repeat-repair", "repeat-repair"
	return broker, native, attach, release
}

func TestRepeatedAttachPersistsLatestVolumeAndBoot(t *testing.T) {
	broker, native, attach, release := retainedFixture(t)
	var lastVolume string
	for _, suffix := range []string{"1", "2", "3"} {
		native.bootSession = "boot-" + suffix
		attach.RequestID = "repeat-attach-" + suffix
		result := broker.Handle(context.Background(), "S-1-5-21-test", attach)
		if !result.OK {
			t.Fatalf("attach: %+v", result)
		}
		journal, err := broker.store.ReadLease(release.LeaseID)
		if err != nil {
			t.Fatal(err)
		}
		if journal.VolumeGUID == lastVolume || journal.VolumeGUID != result.Lease.VolumeGUID ||
			journal.PhysicalPath != result.Lease.PhysicalPath || journal.BootSessionID != native.bootSession || journal.State != "ready" {
			t.Fatalf("stale attachment journal: %+v, lease: %+v", journal, result.Lease)
		}
		lastVolume = journal.VolumeGUID
		release.RequestID = "repeat-release-" + suffix
		if result := broker.Handle(context.Background(), "S-1-5-21-test", release); !result.OK {
			t.Fatalf("retain: %+v", result)
		}
		// A fresh broker must use the persisted identity, rather than its old
		// in-memory session. This is what the old single-attach test missed.
		broker, err = NewBroker(broker.config, native)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestAttachCheckpointFailurePreservesRetainedChild(t *testing.T) {
	broker, native, attach, release := retainedFixture(t)
	journal, err := broker.store.ReadLease(release.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	leasePath, err := broker.store.paths.Lease(release.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	native.beforeAttachCheckpoint = func() {
		if err := os.Remove(leasePath); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(leasePath, 0700); err != nil {
			t.Fatal(err)
		}
	}
	result := broker.Handle(context.Background(), "S-1-5-21-test", attach)
	if result.OK {
		t.Fatal("attach succeeded without a durable checkpoint")
	}
	if _, err := os.Stat(journal.ChildPath); err != nil {
		t.Fatalf("retained child lost: %v", err)
	}
	if _, err := broker.store.ReadRetained(attach.RunID); err != nil {
		t.Fatalf("retained record lost: %v", err)
	}
	if len(broker.children) != 0 {
		t.Fatal("failed attach published an active session")
	}
}

func TestRetainedIdentityFailureHasStableCode(t *testing.T) {
	broker, native, attach, _ := retainedFixture(t)
	native.attachErr = ErrRetainedMountIdentityMismatch
	result := broker.Handle(context.Background(), "S-1-5-21-test", attach)
	if result.OK || result.Error.Code != "retained-mount-identity-mismatch" {
		t.Fatalf("response: %+v", result)
	}
}
