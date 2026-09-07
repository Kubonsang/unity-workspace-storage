# Retained volume identity recovery

The original retained-attach path kept the acquire-time volume GUID after a
successful reattach. If Windows returned a different volume GUID, the next
repair rejected the leftover mount against the old journal identity.

Reattach now checkpoints the resolved physical path and volume GUID before
mounting, and commits the returned session identity and current boot identity
before reporting success. The same transaction is used when a removal needs
to reattach a retained child. Journal failure retains the child. Releasing the
same retained owner again is idempotent; another owner's record is rejected.
Retained lifecycle operations for one run are serialized by a reservation.

Legacy GUID mismatches retain the strict cleanup guard. Recovery first verifies
the recorded child file identity and parent relationship, then attaches only that
detached image read-only without a drive letter. Only a volume belonging to that
exact image can authorize cleanup of the observed old mount. After detaching the
probe, the file identity, mount target and detached state are checked again.
Unproven ownership returns `retained-mount-identity-mismatch`; it never authorizes
replacing the journal GUID with an arbitrary directory target.

The native interface adds a reattach checkpoint callback. Wire protocol 3 and
existing journal/retained schemas remain compatible.

Validation covers repeated reattach with changed GUIDs and boot identities,
checkpoint failure with retained-child preservation, stable error mapping,
ordinary-directory preservation, invalid file identity, all package tests,
`go vet`, and the race detector. Elevated Windows testing of the actual read-only
probe, mismatched foreign volumes, and repeated reboot/repair remains a deployment
gate; unit tests alone do not qualify an installed service replacement.

The frozen extraction manifest is unchanged. The post-RC overlay now records
this patch and existing post-extraction drift in the hb8 baseline (including
pipe/protocol/service changes); the verifier still checks exact hashes and an
explicit overlay entry count.
