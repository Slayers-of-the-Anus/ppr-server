@echo off
cd /d %~dp0
if not exist .env copy .env.example .env >nul
go run .\cmd\server
