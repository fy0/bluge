param(
    [Parameter(Mandatory)][string]$CandidateBinary,
    [Parameter(Mandatory)][string]$OutputPath,
    [string]$BaselineBinary,
    [ValidateRange(1, 20)][int]$Runs = 3
)

$ErrorActionPreference = 'Stop'
$candidate = (Resolve-Path -LiteralPath $CandidateBinary).Path
$baseline = $null
if ($BaselineBinary) { $baseline = (Resolve-Path -LiteralPath $BaselineBinary).Path }
if (Test-Path -LiteralPath $OutputPath) { throw "Output already exists: $OutputPath" }
$out = [IO.Path]::GetFullPath($OutputPath)
[void][IO.Directory]::CreateDirectory([IO.Path]::GetDirectoryName($out))
$oldParallelism = $env:GOMAXPROCS
$env:GOMAXPROCS = '2'
$results = [Collections.Generic.List[object]]::new()

function Measure-Case($binary, $variant, $scenario, $profile, $mib, $extraArgs) {
    $arguments = @('-test.run=^TestWriterMemoryExperiment$', '-test.v', '-test.timeout=5m',
        '-writer-memory-case', $scenario, '-writer-memory-profile', $profile, '-writer-memory-mib', "$mib") + $extraArgs
    $lines = @(& $binary @arguments 2>&1)
    if ($LASTEXITCODE -ne 0) { throw ($lines -join "`n") }
    $line = $lines | Where-Object { $_ -like 'WRITER_MEMORY_RESULT *' } | Select-Object -First 1
    if (!$line) { throw "Missing result: $lines" }
    $result = $line.Substring('WRITER_MEMORY_RESULT '.Length) | ConvertFrom-Json
    $result | Add-Member -NotePropertyName variant -NotePropertyValue $variant
    $result | Add-Member -NotePropertyName trial -NotePropertyValue $trial
    $results.Add($result)
    Write-Host ("trial {0} {1}/{2}/{3}: {4:N1} MiB peak, {5:N2} s including close" -f
        $trial, $variant, $scenario, $profile, ($result.peak_rss_bytes / 1MB), (($result.write_ms + $result.close_ms) / 1000))
}

try {
    for ($trial = 1; $trial -le $Runs; $trial++) {
        # One fresh process per case. Keep comparable runs close together and
        # never run benchmarks concurrently (CPU, disk and page cache are shared).
        foreach ($dense in @($true, $false)) {
            $mib = 32
            $extra = @()
            if ($dense) { $mib = 64; $extra = @('-writer-memory-dense') }
            if ($baseline) { Measure-Case $baseline 'before' 'stream' 'balanced' $mib $extra }
            Measure-Case $candidate 'after' 'stream' 'edge' $mib $extra
        }
        if ($baseline) { Measure-Case $baseline 'before' 'parallel' 'default' 32 @() }
        Measure-Case $candidate 'after' 'parallel' 'edge' 32 @('-writer-memory-lazy')
        Measure-Case $candidate 'after' 'parallel' 'edge-burst' 32 @('-writer-memory-lazy')
        Measure-Case $candidate 'after' 'parallel' 'edge' 32 @('-writer-memory-producers', '64')
        Measure-Case $candidate 'after' 'parallel' 'edge' 32 @('-writer-memory-producers', '64', '-writer-memory-lazy')
    }
    $results | ConvertTo-Json -Depth 6 | Set-Content -LiteralPath $out -Encoding utf8
} finally {
    $env:GOMAXPROCS = $oldParallelism
}
