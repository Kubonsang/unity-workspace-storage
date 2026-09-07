[CmdletBinding()]
param(
    [Parameter()]
    [string]$OverlayManifest
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$moduleRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$manifestPath = Join-Path $moduleRoot 'provenance\frozen-destination-sha256.tsv'
$overlayManifestPath = if ([string]::IsNullOrWhiteSpace($OverlayManifest)) {
    $null
} else {
    [IO.Path]::GetFullPath($OverlayManifest)
}
$expectedFrozenFileCount = 42
$expectedOverlayFileCount = 20
$failures = [Collections.Generic.List[string]]::new()
$checked = 0
$sha256 = [Security.Cryptography.SHA256]::Create()

function Read-DestinationManifest([string]$Path) {
    $result = [Collections.Generic.List[object]]::new()
    $seen = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
    foreach ($line in Get-Content -LiteralPath $Path) {
        if ([string]::IsNullOrWhiteSpace($line) -or $line.StartsWith('#')) {
            continue
        }
        $parts = $line -split "`t"
        if ($parts.Count -ne 2) {
            throw "Invalid destination manifest line in ${Path}: $line"
        }
        $expected, $relativePath = $parts
        $segments = $relativePath -split '[/\\]'
        if ([IO.Path]::IsPathRooted($relativePath) -or $segments -contains '..') {
            throw "Unsafe destination path: $relativePath"
        }
        if (-not $seen.Add($relativePath)) {
            throw "Duplicate destination path in ${Path}: $relativePath"
        }
        $result.Add([pscustomobject]@{ Expected = $expected; RelativePath = $relativePath })
    }
    return $result
}

$frozenEntries = @(Read-DestinationManifest $manifestPath)
if ($frozenEntries.Count -ne $expectedFrozenFileCount) {
    $failures.Add("frozen destination manifest count: expected=$expectedFrozenFileCount actual=$($frozenEntries.Count)")
}
$expectedByPath = [Collections.Specialized.OrderedDictionary]::new([StringComparer]::OrdinalIgnoreCase)
foreach ($entry in $frozenEntries) {
    $expectedByPath.Add($entry.RelativePath, $entry.Expected)
}

$overlayEntries = @()
if ($null -ne $overlayManifestPath) {
    if (-not (Test-Path -LiteralPath $overlayManifestPath -PathType Leaf)) {
        throw "Overlay destination manifest does not exist: $overlayManifestPath"
    }
    $overlayEntries = @(Read-DestinationManifest $overlayManifestPath)
    if ($overlayEntries.Count -ne $expectedOverlayFileCount) {
        $failures.Add("overlay destination manifest count: expected=$expectedOverlayFileCount actual=$($overlayEntries.Count)")
    }
    foreach ($entry in $overlayEntries) {
        $expectedByPath[$entry.RelativePath] = $entry.Expected
    }
}

try {
    foreach ($entry in $expectedByPath.GetEnumerator()) {
        $relativePath = [string]$entry.Key
        $expected = [string]$entry.Value
        $path = Join-Path $moduleRoot ($relativePath.Replace('/', '\'))
        if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
            $failures.Add("missing provider destination: $relativePath")
            continue
        }
        $normalized = [IO.File]::ReadAllText($path).Replace("`r`n", "`n")
        $bytes = [Text.UTF8Encoding]::new($false).GetBytes($normalized)
        $actual = ([BitConverter]::ToString($sha256.ComputeHash($bytes))).Replace('-', '').ToLowerInvariant()
        if ($actual -ne $expected) {
            $failures.Add("provider destination drift: $relativePath expected=$expected actual=$actual")
            continue
        }
        $checked++
    }
} finally {
    $sha256.Dispose()
}

if ($failures.Count -ne 0) {
    $failures | ForEach-Object { Write-Error $_ }
    throw "provider destination parity failed: $($failures.Count) mismatch(es)"
}

[ordered]@{
    schemaVersion = 1
    status = 'PASS'
    filesChecked = $checked
    frozenFiles = $frozenEntries.Count
    overlayFiles = $overlayEntries.Count
} | ConvertTo-Json
