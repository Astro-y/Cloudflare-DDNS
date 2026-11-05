[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
Push-Location -LiteralPath $projectRoot
try {
    Write-Host '检查 Go 格式…'
    $files = Get-ChildItem -LiteralPath $projectRoot -Filter '*.go' -File
    $unformatted = gofmt -l $files.FullName
    if ($LASTEXITCODE -ne 0) { throw 'gofmt 执行失败。' }
    if ($unformatted) { throw "以下文件需要 gofmt：`n$($unformatted -join "`n")" }
    Write-Host '运行测试…'
    go test -mod=readonly ./...
    if ($LASTEXITCODE -ne 0) { throw 'Go 测试失败。' }
    Write-Host '运行静态检查…'
    go vet -mod=readonly ./...
    if ($LASTEXITCODE -ne 0) { throw 'Go 静态检查失败。' }
} finally {
    Pop-Location
}
