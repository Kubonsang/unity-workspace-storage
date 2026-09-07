package workspace

import (
	"context"
	"errors"
	"fmt"
)

// Persist the new volume identity before exposing its directory mount. In
// particular, a second repair must not compare against the initial acquire's
// GUID. Retained children survive every failure in this transaction.
func (b *Broker) attachRecorded(ctx context.Context, parent ParentMetadata, journal *LeaseJournal) (ChildSession, Metrics, error) {
	transition := func(state, physical, volume string) error {
		if volume == "" || physical == "" {
			return fmt.Errorf("attach did not supply a complete volume identity")
		}
		journal.State = state
		journal.VolumeGUID = volume
		journal.PhysicalPath = physical
		journal.BootSessionID = b.native.BootSessionID()
		return b.store.WriteLease(*journal)
	}
	session, metrics, err := b.native.AttachChild(ctx, parent, *journal, transition)
	if err != nil {
		return nil, metrics, err
	}
	info := session.Info()
	if info.LeaseID != journal.LeaseID || info.RunID != journal.RunID ||
		!samePath(info.MountPath, journal.MountPath) || session.FileIdentity() != journal.FileIdentity {
		_, cleanupErr := session.Release(context.Background(), false)
		return nil, metrics, errors.Join(ErrOwnershipMismatch, cleanupErr)
	}
	if err := transition("ready", info.PhysicalPath, info.VolumeGUID); err != nil {
		_, cleanupErr := session.Release(context.Background(), false)
		return nil, metrics, errors.Join(err, cleanupErr)
	}
	return session, metrics, nil
}
