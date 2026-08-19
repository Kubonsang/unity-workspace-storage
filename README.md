# unity-workspace-storage

`unity-workspace-storage` is the repository-ready extraction of the VHDX and
workspace-storage provider shipped in `testplay-runner v0.13.0-rc.1`.

The first extraction intentionally favors behavioral parity over redesign:

- `storage` is the native CoW layer (Differencing VHDX on Windows, with the
  already-existing APFS/reflink provider code retained).
- `workspace` is the schema-2 broker, immutable-parent store, lease journal,
  quota/GC, crash recovery, and authenticated named-pipe transport.
- `contract` is the small consumer-neutral `acquire / status / release` API.
- `contract/v2` is the provider-neutral Windows/macOS/Linux lifecycle API.
- `cmd/unity-workspace-storage` exposes that contract as JSON for non-Go
  consumers such as HoneyBee.

No automatic provider selection, new fallback, parent format change, or broad
refactor is part of this extraction.

## Public lifecycle contract

The public schema version is `1`. An operator must first run the extracted
broker and provision a compatible immutable parent through the preserved
schema-2 control plane. Before acquire, the consumer creates its physical
workspace shell under the broker-owned workspace root; its `Library` path must
not exist. The broker alone derives and mounts that path.

Go consumers use `contract.Service`:

```go
service := contract.Default()

acquired, err := service.Acquire(ctx, contract.AcquireRequest{
    SchemaVersion: contract.SchemaVersion,
    ConsumerID:    "honeybee-job-42",
    WorkspaceID:   "honeybee-workspace-42",
    ParentKey:     parentKey,
    ClientPID:     os.Getpid(),
})

status, err := service.Status(ctx, contract.StatusRequest{
    SchemaVersion: contract.SchemaVersion,
})

released, err := service.Release(ctx, contract.ReleaseRequest{
    SchemaVersion: contract.SchemaVersion,
    LeaseID:       acquired.Lease.LeaseID,
})
```

`ConsumerID` is mapped at the adapter boundary to the frozen provider's legacy
`runId` field. It is an opaque consumer/job identity; callers do not need any
TestPlay type or package.

The command-line contract emits exactly one JSON value on stdout:

```powershell
Get-Content .\acquire.json -Raw |
  unity-workspace-storage workspace acquire

unity-workspace-storage workspace status `
  --request-id honeybee-status-42

unity-workspace-storage workspace release `
  --lease-id lease-... `
  --request-id honeybee-release-42
```

Example `acquire.json`:

```json
{
  "schemaVersion": 1,
  "requestId": "honeybee-acquire-42",
  "consumerId": "honeybee-job-42",
  "workspaceId": "honeybee-workspace-42",
  "parentKey": {
    "schemaVersion": 2,
    "digest": "<64-lowercase-hex>",
    "libraryKey": {},
    "provider": "vhdx-differencing",
    "filesystem": "NTFS",
    "virtualBytes": 68719476736,
    "blockBytes": 2097152,
    "sectorBytes": 4096
  },
  "clientPid": 1234
}
```

The complete `parentKey` must be the same value used to commit the immutable
parent. Supplying only its digest is not accepted by the provider.

## Build and parity checks

```powershell
go test ./... -count=1
go vet ./...
powershell.exe -NoProfile -ExecutionPolicy Bypass `
  -File .\scripts\verify-source-parity.ps1 `
  -SourceRoot ..
```

The copied provider tests remain in `workspace` and `storage`. The parity
script verifies every mapped file against the frozen Git blob after only the
documented package/import disentangling transformations.

See [PROVENANCE.md](PROVENANCE.md) for the exact checkpoint and deliberate
compatibility debt.

## macOS and Linux CoW daemon

Schema 2 adds immutable directory parents, APFS clonefile/Linux reflink child
workspaces, quota reporting, ownership-safe cleanup, and daemon restart
recovery. The daemon runs as the current user over an authenticated Unix domain
socket. A filesystem that cannot perform a native CoW clone returns
`cow-unavailable`; there is no physical-copy fallback.

See [docs/unix-daemon.md](docs/unix-daemon.md) for configuration and the
parent producer plus workspace consumer flow. Windows schema 1 and its frozen
named-pipe broker remain unchanged.
