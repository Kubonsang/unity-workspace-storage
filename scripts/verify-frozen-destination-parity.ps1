[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$moduleRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$manifestPath = Join-Path $moduleRoot 'provenance\frozen-destination-sha256.tsv'
$expectedFileCount = 42
$failures = [Collections.Generic.List[string]]::new()
$seen = [Collections.Generic.HashSet[string]]::new([StringComparer]::OrdinalIgnoreCase)
$entries = 0
$checked = 0
$sha256 = [Security.Cryptography.SHA256]::Create()

try {
    foreach ($line in Get-Content -LiteralPath $manifestPath) {
        if ([string]::IsNullOrWhiteSpace($line) -or $line.StartsWith('#')) {
            continue
        }
        $parts = $line -split "`t"
        if ($parts.Count -ne 2) {
            throw "Invalid frozen destination manifest line: $line"
        }
        $expected, $relativePath = $parts
        $segments = $relativePath -split '[/\\]'
        if ([IO.Path]::IsPathRooted($relativePath) -or $segments -contains '..') {
            throw "Unsafe frozen destination path: $relativePath"
        }
        if (-not $seen.Add($relativePath)) {
            throw "Duplicate frozen destination path: $relativePath"
        }
        $entries++
        $path = Join-Path $moduleRoot ($relativePath.Replace('/', '\'))
        if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
            $failures.Add("missing frozen destination: $relativePath")
            continue
        }
        $normalized = [IO.File]::ReadAllText($path).Replace("`r`n", "`n")
        $bytes = [Text.UTF8Encoding]::new($false).GetBytes($normalized)
        $actual = [Convert]::ToHexString($sha256.ComputeHash($bytes)).ToLowerInvariant()
        if ($actual -ne $expected) {
            $failures.Add("frozen destination drift: $relativePath expected=$expected actual=$actual")
            continue
        }
        $checked++
    }
} finally {
    $sha256.Dispose()
}

if ($entries -ne $expectedFileCount) {
    $failures.Add("frozen destination manifest count: expected=$expectedFileCount actual=$entries")
}

if ($failures.Count -ne 0) {
    $failures | ForEach-Object { Write-Error $_ }
    throw "frozen destination parity failed: $($failures.Count) mismatch(es)"
}

[ordered]@{
    schemaVersion = 1
    status = 'PASS'
    filesChecked = $checked
} | ConvertTo-Json
