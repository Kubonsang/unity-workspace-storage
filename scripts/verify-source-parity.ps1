[CmdletBinding()]
param(
    [Parameter()]
    [string]$SourceRoot = (Join-Path $PSScriptRoot '..\..'),

    [Parameter()]
    [string]$Checkpoint = 'beabf36a299572607232806807ad9b9c2d4cb222',

    [Parameter()]
    [string]$OverlayManifest = '',

    [Parameter()]
    [string]$OverlayCheckpoint = '5d5cbe16dd2da2e58365b311d99b87e87598c09f'
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$moduleRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$sourceRoot = [IO.Path]::GetFullPath($SourceRoot)
$manifestPath = Join-Path $moduleRoot 'provenance\source-map.tsv'
$overlayManifestPath = if ([string]::IsNullOrWhiteSpace($OverlayManifest)) {
    Join-Path $moduleRoot 'provenance\post-rc-source-map.tsv'
} else {
    [IO.Path]::GetFullPath($OverlayManifest)
}

if (-not (Test-Path -LiteralPath (Join-Path $sourceRoot '.git'))) {
    throw "SourceRoot is not the testplay-runner checkout: $sourceRoot"
}

function Normalize-LineEndings([string]$Value) {
    return $Value.Replace("`r`n", "`n")
}

function Transform-ProviderSource([string]$Path, [string]$Value) {
    $result = Normalize-LineEndings $Value
    if ($Path.StartsWith('internal/vhdxworkspace/')) {
        $result = $result.Replace('package vhdxworkspace', 'package workspace')
        $result = $result.Replace('github.com/Kubonsang/testplay-runner/internal/atomicfile', 'github.com/Kubonsang/unity-workspace-storage/internal/atomicfile')
        $result = $result.Replace('github.com/Kubonsang/testplay-runner/internal/libraryimage', 'github.com/Kubonsang/unity-workspace-storage/internal/libraryimage')
        $result = $result.Replace('github.com/Kubonsang/testplay-runner/internal/shadow', 'github.com/Kubonsang/unity-workspace-storage/internal/filecopy')
        $result = $result.Replace('github.com/Kubonsang/testplay-runner/internal/vhdxstorage', 'github.com/Kubonsang/unity-workspace-storage/storage')
        $result = $result.Replace('shadow.CopyDirParallel', 'filecopy.CopyDirParallel')
        $result = $result.Replace('vhdxstorage.', 'storage.')
        return $result
    }
    if ($Path.StartsWith('internal/vhdxstorage/')) {
        $result = $result.Replace('package vhdxstorage', 'package storage')
        $result = $result.Replace('github.com/Kubonsang/testplay-runner/internal/shadow', 'github.com/Kubonsang/unity-workspace-storage/internal/fileusage')
        $result = $result.Replace('shadow.MeasureDirectoryUsage', 'fileusage.MeasureDirectoryUsage')
        return $result
    }
    throw "Unexpected provider source: $Path"
}

function Add-SourceMap(
    [Collections.Specialized.OrderedDictionary]$Entries,
    [string]$Path,
    [string]$EntryCheckpoint,
    [bool]$IsOverlay
) {
    $seenInManifest = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
    foreach ($line in Get-Content -LiteralPath $Path) {
        if ([string]::IsNullOrWhiteSpace($line) -or $line.StartsWith('#')) {
            continue
        }
        $parts = $line -split "`t"
        if ($parts.Count -ne 3) {
            throw "Invalid source map line in ${Path}: $line"
        }
        $expectedBlob, $sourcePath, $destinationPath = $parts
        if (-not $seenInManifest.Add($destinationPath)) {
            throw "Duplicate destination path in ${Path}: $destinationPath"
        }
        if ($IsOverlay -and $Entries.Contains($destinationPath)) {
            if ($Entries[$destinationPath].SourcePath -ne $sourcePath) {
                throw "Overlay remaps frozen destination ${destinationPath}: expected=$($Entries[$destinationPath].SourcePath) actual=$sourcePath"
            }
            $Entries[$destinationPath] = [pscustomobject]@{
                ExpectedBlob = $expectedBlob
                SourcePath = $sourcePath
                DestinationPath = $destinationPath
                Checkpoint = $EntryCheckpoint
                IsOverlay = $true
            }
            continue
        }
        if ($Entries.Contains($destinationPath)) {
            throw "Duplicate destination path in source map: $destinationPath"
        }
        $Entries.Add($destinationPath, [pscustomobject]@{
            ExpectedBlob = $expectedBlob
            SourcePath = $sourcePath
            DestinationPath = $destinationPath
            Checkpoint = $EntryCheckpoint
            IsOverlay = $IsOverlay
        })
    }
}

$entries = [Collections.Specialized.OrderedDictionary]::new([StringComparer]::OrdinalIgnoreCase)
Add-SourceMap $entries $manifestPath $Checkpoint $false
if ($null -ne $overlayManifestPath) {
    if (-not (Test-Path -LiteralPath $overlayManifestPath -PathType Leaf)) {
        throw "Overlay source map does not exist: $overlayManifestPath"
    }
    Add-SourceMap $entries $overlayManifestPath $OverlayCheckpoint $true
}

$failures = [Collections.Generic.List[string]]::new()
$checked = 0
$overlayChecked = 0
foreach ($entry in $entries.Values) {
    $expectedBlob = $entry.ExpectedBlob
    $sourcePath = $entry.SourcePath
    $destinationPath = $entry.DestinationPath
    $entryCheckpoint = $entry.Checkpoint
    $actualBlob = (& git -c "safe.directory=$($sourceRoot.Replace('\', '/'))" -C $sourceRoot rev-parse "${entryCheckpoint}:$sourcePath").Trim()
    if ($LASTEXITCODE -ne 0 -or $actualBlob -ne $expectedBlob) {
        $failures.Add("checkpoint blob mismatch: checkpoint=$entryCheckpoint path=$sourcePath expected=$expectedBlob actual=$actualBlob")
        continue
    }
    $sourceText = (& git -c "safe.directory=$($sourceRoot.Replace('\', '/'))" -C $sourceRoot show "${entryCheckpoint}:$sourcePath") -join "`n"
    if ($LASTEXITCODE -ne 0) {
        $failures.Add("cannot read checkpoint source: $sourcePath")
        continue
    }
    $expected = Transform-ProviderSource $sourcePath ($sourceText + "`n")
    $destination = Join-Path $moduleRoot ($destinationPath.Replace('/', '\'))
    if (-not (Test-Path -LiteralPath $destination)) {
        $failures.Add("missing extracted file: $destinationPath")
        continue
    }
    $actual = Normalize-LineEndings ([IO.File]::ReadAllText($destination))
    if ($actual -ne $expected) {
        $failures.Add("content drift: $sourcePath -> $destinationPath")
        continue
    }
    $checked++
    if ($entry.IsOverlay) {
        $overlayChecked++
    }
}

if ($failures.Count -ne 0) {
    $failures | ForEach-Object { Write-Error $_ }
    throw "provider source parity failed: $($failures.Count) mismatch(es)"
}

[ordered]@{
    schemaVersion = 1
    status = 'PASS'
    frozenCheckpoint = $Checkpoint
    overlayCheckpoint = if ($null -eq $overlayManifestPath) { $null } else { $OverlayCheckpoint }
    filesChecked = $checked
    overlayFilesChecked = $overlayChecked
} | ConvertTo-Json
