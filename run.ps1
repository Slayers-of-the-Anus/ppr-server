Set-Location $PSScriptRoot
if (-not (Test-Path .env)) { Copy-Item .env.example .env }
go run ./cmd/server
