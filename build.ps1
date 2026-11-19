# Сборка MAIRDI Загрузчика: .\build.ps1  →  bin\MairdiDownloader.exe
$ErrorActionPreference = 'Stop'
Set-Location "$PSScriptRoot\app"
if (-not (Test-Path icon.ico)) { go run ./cmd/gen }
$env:CGO_ENABLED = '0'
go build -trimpath -ldflags "-H windowsgui -s -w" -o "$PSScriptRoot\bin\MairdiDownloader.exe" .
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
Write-Host "Готово: $PSScriptRoot\bin\MairdiDownloader.exe"
