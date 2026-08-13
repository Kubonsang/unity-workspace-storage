[CmdletBinding()]
param(
    [Parameter()]
    [string]$SourceRoot = (Join-Path $PSScriptRoot '..\..'),

    [Parameter()]
    [string]$Checkpoint = 'beabf36a299572607232806807ad9b9c2d4cb222'
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$moduleRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$sourceRoot = [IO.Path]::GetFullPath($SourceRoot)
$manifestPath = Join-Path $moduleRoot 'provenance\source-map.tsv'

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

$failures = [Collections.Generic.List[string]]::new()
$checked = 0
foreach ($line in Get-Content -LiteralPath $manifestPath) {
    if ([string]::IsNullOrWhiteSpace($line) -or $line.StartsWith('#')) {
        continue
    }
    $parts = $line -split "`t"
    if ($parts.Count -ne 3) {
        throw "Invalid source map line: $line"
    }
    $expectedBlob, $sourcePath, $destinationPath = $parts
    $actualBlob = (& git -c "safe.directory=$($sourceRoot.Replace('\', '/'))" -C $sourceRoot rev-parse "${Checkpoint}:$sourcePath").Trim()
    if ($LASTEXITCODE -ne 0 -or $actualBlob -ne $expectedBlob) {
        $failures.Add("checkpoint blob mismatch: $sourcePath expected=$expectedBlob actual=$actualBlob")
        continue
    }
    $sourceText = (& git -c "safe.directory=$($sourceRoot.Replace('\', '/'))" -C $sourceRoot show "${Checkpoint}:$sourcePath") -join "`n"
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
}

if ($failures.Count -ne 0) {
    $failures | ForEach-Object { Write-Error $_ }
    throw "provider source parity failed: $($failures.Count) mismatch(es)"
}

[ordered]@{
    schemaVersion = 1
    status = 'PASS'
    checkpoint = $Checkpoint
    filesChecked = $checked
} | ConvertTo-Json
