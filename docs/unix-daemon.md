# macOS and Linux user daemon

The schema-2 daemon owns one directory-clone store for one OS user. It runs
without elevation and never falls back to a physical copy. macOS requires APFS;
Linux requires a filesystem on which `cp --reflink=always` succeeds, such as a
reflink-enabled XFS or Btrfs volume.

## Configuration

```json
{
  "schemaVersion": 1,
  "storeRoot": "/absolute/path/workspace-storage",
  "workspaceRoot": "/absolute/path/workspaces",
  "socketPath": "/absolute/path/runtime/unity-workspace-storage.sock",
  "quotaBytes": 34359738368,
  "hostFloorBytes": 5368709120,
  "childReserveBytes": 2147483648
}
```

All three paths must be absolute. The socket directory and socket are created
with modes `0700` and `0600`. Linux `SO_PEERCRED` and macOS `LOCAL_PEERCRED`
restrict requests to the daemon UID.

Run in the foreground:

```sh
unity-workspace-storage serve --config /absolute/path/config.json
```

Clients use `UNITY_WORKSPACE_STORAGE_SOCKET` when the configured socket differs
from the platform default.

## Producer and consumer flow

```sh
KEY=$(printf '%s' 'producer compatibility inputs' | sha256sum | cut -d' ' -f1)
unity-workspace-storage parent begin --compatibility-key "$KEY"
# Populate the returned stagingPath without symlinks or special files.
unity-workspace-storage parent commit --transaction-id txn-...

unity-workspace-storage workspace acquire --request acquire-v2.json
unity-workspace-storage workspace status --schema 2
unity-workspace-storage workspace release --schema 2 --lease-id lease-...
```

The producer chooses the compatibility key. The daemon computes and records the
actual committed content digest plus filesystem device/inode identity,
revalidates both before every acquire, and uses an opaque provider-specific
parent ID for consumers. Replaced or modified parents are quarantined.

Example acquire request:

```json
{
  "schemaVersion": 2,
  "operation": "workspace-acquire",
  "requestId": "honeybee-acquire-42",
  "consumerId": "honeybee-job-42",
  "workspaceId": "honeybee-workspace-42",
  "parentId": "parent-0123456789abcdef0123456789abcdef",
  "clientPid": 1234
}
```

The producer creates `workspaceRoot/workspaceId` before acquire; `Library` must
not exist. Release removes only a workspace carrying the exact daemon-authored
owner marker and exact lease identity.

## Service manager examples

The initial release intentionally does not install services. Templates in
`contrib/systemd` and `contrib/launchd` show how to wrap the foreground daemon;
replace every placeholder with an absolute local path.
