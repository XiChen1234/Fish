# Fish 一键交叉编译脚本（Windows 开发机 → 各平台产物）
$ErrorActionPreference = "Stop"
$env:Path = [System.Environment]::GetEnvironmentVariable("Path","Machine") + ";" + [System.Environment]::GetEnvironmentVariable("Path","User")
$env:GOPROXY = "https://goproxy.cn,direct"
$env:CGO_ENABLED = "0"   # 纯 Go（modernc.org/sqlite），无 CGO 依赖

$out = Join-Path $PSScriptRoot "..\dist"
New-Item -ItemType Directory -Force -Path $out | Out-Null

$targets = @(
    @{ GOOS = "linux";   GOARCH = "amd64"; Name = "fish-server-linux-amd64" },
    @{ GOOS = "linux";   GOARCH = "arm64"; Name = "fish-server-linux-arm64" },
    @{ GOOS = "windows"; GOARCH = "amd64"; Name = "fish-server-windows-amd64.exe" }
)

foreach ($t in $targets) {
    $env:GOOS = $t.GOOS
    $env:GOARCH = $t.GOARCH
    $dst = Join-Path $out $t.Name
    go build -trimpath -ldflags "-s -w" -o $dst .\cmd\server
    if ($LASTEXITCODE -ne 0) { throw "构建失败: $($t.Name)" }
    Write-Output "OK  $dst"
}

Write-Output "全部完成，产物见 dist\"
