#!/usr/bin/env pwsh
# build.ps1 — Builds server-monitor.exe for Windows x64
# Run this in the project root after installing Go (https://go.dev/dl/)

$ErrorActionPreference = "Stop"

Write-Host "`n[BUILD] Building server-monitor.exe (Windows / amd64) ..." -ForegroundColor Cyan

$env:CGO_ENABLED = "0"
$env:GOOS        = "windows"
$env:GOARCH      = "amd64"

go build -ldflags="-s -w" -trimpath -o server-monitor.exe .

if ($LASTEXITCODE -eq 0) {
    $size = [math]::Round((Get-Item server-monitor.exe).Length / 1MB, 1)
    Write-Host "[OK] Built: server-monitor.exe  ($size MB)`n" -ForegroundColor Green
    Write-Host "Run it:" -ForegroundColor Yellow
    Write-Host "  .\server-monitor.exe`n"
    Write-Host "Then open: http://localhost:8266" -ForegroundColor Cyan
} else {
    Write-Host "[ERROR] Build failed." -ForegroundColor Red
}
