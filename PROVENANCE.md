# Extraction provenance

## HoneyBee child geometry follow-up (2026-09-08)

Runtime change `c238f283ded29f716f72e7d556cfeef3efd98639` explicitly selects
1 MiB blocks for newly created Windows differencing children. Existing 2 MiB
parents and retained children keep their geometry. The normalized hashes for
`storage/lifecycle.go` and `storage/lifecycle_windows.go` are updated only in
the destination overlay; the frozen extraction manifests remain unchanged.
The additive native geometry test is not a checkpoint-derived file.

HoneyBee's GNF experiment (three fresh children per geometry, two Unity sessions
each) measured median final allocation of 940,572,672 versus 763,363,328 bytes
(18.84% less), without a timing regression against its pre-registered gate.
Verification used read-only mounts after measurement. Offline compaction
reclaimed zero bytes on both disposable copies. These results cover one project
and host; they do not establish universal savings or long-term growth.

The hb10 runtime identifier remains `0.0.0+c238f283ded2.hb10`. A subsequent
provenance-only source pin records these overlay hashes without changing runtime
Go source or the compiled broker payload.

## Source identity

- source repository: `github.com/Kubonsang/testplay-runner`
- released candidate: `v0.13.0-rc.1`
- RC tag target: `cd8e34c4ec8ce0c01231ea17ab12a2a3c388aa0d`
- release-preparation base: `0bd5dde4da3b92be8a41fa9a98990aafe17b665b`
- **frozen provider checkpoint:**
  `beabf36a299572607232806807ad9b9c2d4cb222`
- post-RC synchronized security checkpoint:
  `5d5cbe16dd2da2e58365b311d99b87e87598c09f`
- preflight-validated head:
  `d7b772cbb4dbd00a79a1a9af0d5fd9f10e8e9c5f`
- extraction started from observation head:
  `fd2d4b4` (`codex/release-v0.13.0-rc.1`)

Before extraction, `git diff --exit-code beabf36 -- internal/vhdxworkspace
internal/vhdxstorage` returned success at the observation head. The individual
checkpoint blob IDs and destinations are committed in
`provenance/source-map.tsv`. That 42-file frozen map remains unchanged. The
five-file synchronized security delta is recorded separately in
`provenance/post-rc-source-map.tsv`.

## Mapping and allowed transformations

| TestPlay source | Extracted responsibility |
|---|---|
| `internal/vhdxstorage` | public `storage` package |
| `internal/vhdxworkspace` | public `workspace` package |
| `internal/atomicfile` | private crash-safe file primitive |
| `internal/libraryimage/key.go` | private compatibility-key primitive |
| `internal/shadow` copy/usage functions | private file copy/usage primitives only |

The provider files receive only deterministic extraction transformations:

1. `package vhdxworkspace` becomes `package workspace`;
2. `package vhdxstorage` becomes `package storage`;
3. imports are redirected to this module's equivalent private primitives;
4. the corresponding qualifiers (`vhdxstorage`, `shadow`) are renamed.

The consumer-neutral `contract` package and JSON CLI are new adapters outside
the provider core. They translate `consumerId` to the legacy schema-2 `runId`
without changing the broker wire representation.

## Deliberately preserved compatibility identifiers

The first extraction keeps these historical values because changing any of
them would invalidate RC parity or existing recovery artifacts:

- provider: `vhdx-differencing`;
- broker protocol schema: `2`;
- parent metadata schema: `2`;
- default pipe: `\\.\pipe\unity-workspace-storage-v2`;
- Windows service name: `UnityWorkspaceStorage`;
- workspace owner marker: `.testplay-vhdx-workspace-owner.json`;
- Unix ownership/quarantine markers and VHDX volume labels carrying the
  historical TestPlay prefix;
- schema-2 fields such as `runId` and existing error/state strings.

Those names are compatibility debt, not new ownership by TestPlay. A later,
separately validated protocol migration may introduce neutral names and dual
read support. It is explicitly out of scope for the parity extraction.

## TestPlay observation freeze

The extraction does not modify or rewire the RC provider, run service, release
scripts, or Day 3/Day 7 observation documents in `testplay-runner`. This is
intentional: replacing the provider during the observation window would make
the published RC evidence incomparable. The standalone module is the new
ownership boundary; switching TestPlay to it is a follow-up after the RC
observation gate, with its own parity evidence.

## Verification record

Run both suites from the source checkout:

```powershell
go test ./internal/vhdxworkspace ./internal/vhdxstorage -count=1
Push-Location .\unity-workspace-storage
go test ./... -count=1
go vet ./...
powershell.exe -NoProfile -ExecutionPolicy Bypass `
  -File .\scripts\verify-source-parity.ps1 -SourceRoot ..
powershell.exe -NoProfile -ExecutionPolicy Bypass `
  -File .\scripts\verify-frozen-destination-parity.ps1 `
  -OverlayManifest .\provenance\post-rc-destination-sha256.tsv
Pop-Location
```

The extraction is not evidence for a new native performance result. Native RC
evidence remains attributed to the frozen checkpoint and the artifacts linked
from `testplay-runner/docs/differencing-vhdx-workspace-provider.md` and
`testplay-runner/docs/32_v0.13.0-rc.1_validation.md`.

### Observed extraction checks (2026-08-13)

| Check | Result |
|---|---|
| frozen provider diff at observation head | PASS — no changes under the two source provider paths |
| checkpoint source parity | PASS — all 42 mapped source/test blobs |
| original provider tests | PASS |
| extracted full tests | PASS |
| extracted race detector | PASS |
| extracted vet | PASS |
| extracted module checksum verification | PASS — all modules verified |
| original `testplay-runner` full tests | PASS |

These are static/local Windows checks. No privileged native VHDX evidence was
rerun because provider bytes are checkpoint-derived and this extraction does
not claim a new native result.

## Post-RC synchronized security fix

The frozen checkpoint and `codex/release-v0.13.0-rc.1` remain unchanged. A
separate TestPlay follow-up passes the already-durable installed-user SID into
writable parent, child, and recovery attaches. This prevents
`AttachVirtualDisk` from inheriting the LocalSystem-owned VHDX file DACL while
leaving public schemas, journal fields, cleanup, quarantine, and recovery
state transitions unchanged.

The synchronized TestPlay source patch is recorded in
[`testplay-runner#47`](https://github.com/Kubonsang/testplay-runner/pull/47).

The extracted provider carries the package-renamed form of that patch and its
opt-in non-elevated VHDX regression test. Four frozen destinations are
explicitly overlaid and one test destination is added. The original manifests
remain intact; current-head verification composes them with the post-RC
source and destination overlay manifests.

### Observed post-RC security checks (2026-08-19)

| Check | Result |
|---|---|
| original frozen source map | UNCHANGED — 42 entries remain anchored at `beabf36a` |
| layered source parity | PASS — 43 active destinations, including 5 post-RC overlay entries |
| layered destination parity | PASS — 42 frozen hashes composed with 5 overlay hashes |
| extracted full tests / vet | PASS |
| changed-package race detector | PASS |
| non-elevated installed-user VHDX write/read/delete/abort | PASS — clean abort and residual counts unchanged |

## Post-extraction schema 2 development

Provider-neutral schema 2, the Unix user daemon, and durable Unix lease
recovery are additive files developed after the frozen extraction. They are
not represented as checkpoint-derived source in either source map. The parity
verifier resolves the unchanged 42-file frozen map plus the five-file post-RC
overlay to 43 active provider destinations. Windows schema 1 remains the RC
compatibility boundary; schema 2 adapters and Unix native evidence carry their
own tests and CI history.

Because CI does not clone the TestPlay source repository, the normalized
destination hashes for those same 42 mapped files are committed in
`provenance/frozen-destination-sha256.tsv`. The original hashes are unchanged;
`provenance/post-rc-destination-sha256.tsv` records the synchronized delta.
Every Windows matrix job composes both manifests and fails if an active
provider destination is missing or drifts. The source-aware verifier remains
the stronger local check when the original repository is available.

The schema-2 daemon selects the additive `storage.NewDaemonBackend`. On Linux
it delegates to the frozen reflink implementation. On macOS its additive
adapter preserves the frozen `clonefileat` tree walk while allowing absolute
paths that traverse system-owned aliases such as `/var -> /private/var`;
source-tree symlinks and special files remain rejected before cloning. The
checkpoint-derived `storage/clone_darwin.go` is unchanged. Native CI separately
requires two-child isolation, allocated-byte evidence, clean release, and
daemon restart recovery on APFS and reflink-enabled XFS.
