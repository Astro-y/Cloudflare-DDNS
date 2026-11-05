[CmdletBinding()]
param(
    [ValidatePattern('^[0-9A-Za-z._+-]+$')]
    [string]$Version = '0.1.0',
    [string]$OutputDirectory = (Join-Path (Split-Path -Parent $PSScriptRoot) 'dist'),
    [switch]$SkipTests
)

$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
$outputPath = [System.IO.Path]::GetFullPath($OutputDirectory)
$saved = @{}
foreach ($name in @('CGO_ENABLED', 'GOOS', 'GOARCH')) {
    $saved[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
}
Push-Location -LiteralPath $projectRoot
try {
    if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
        throw '需要 Go 1.24 或更高版本构建；目标 Linux 主机运行时不需要 Go。'
    }
    if (-not $SkipTests) {
        Write-Host '运行当前主机的 Go 测试和静态检查…'
        # Do not inherit a cross-compilation target for host tests.
        Remove-Item Env:GOOS -ErrorAction SilentlyContinue
        Remove-Item Env:GOARCH -ErrorAction SilentlyContinue
        $env:CGO_ENABLED = '0'
        go test -mod=readonly ./...
        if ($LASTEXITCODE -ne 0) { throw 'Go 测试失败。' }
        go vet -mod=readonly ./...
        if ($LASTEXITCODE -ne 0) { throw 'Go 静态检查失败。' }
    }
    New-Item -ItemType Directory -Path $outputPath -Force | Out-Null
    $env:CGO_ENABLED = '0'
    $env:GOOS = 'linux'
    $checksums = [System.Collections.Generic.List[string]]::new()
    foreach ($architecture in @('amd64', 'arm64')) {
        $env:GOARCH = $architecture
        $fileName = "cfddns-linux-$architecture"
        $target = Join-Path $outputPath $fileName
        Write-Host "构建 $fileName…"
        go build -mod=readonly -trimpath -ldflags "-s -w -X main.version=$Version" -o $target .
        if ($LASTEXITCODE -ne 0) { throw "$architecture 构建失败。" }
        $digest = (Get-FileHash -LiteralPath $target -Algorithm SHA256).Hash.ToLowerInvariant()
        $checksums.Add("$digest  $fileName")
    }
    $sumFile = Join-Path $outputPath 'SHA256SUMS'
    [System.IO.File]::WriteAllText($sumFile, ($checksums -join "`n") + "`n", [System.Text.UTF8Encoding]::new($false))
    Write-Host "完成：$outputPath"
} finally {
    foreach ($name in $saved.Keys) {
        [Environment]::SetEnvironmentVariable($name, $saved[$name], 'Process')
    }
    Pop-Location
}
