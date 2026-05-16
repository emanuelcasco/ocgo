@echo off
setlocal

for /f "delims=" %%v in ('git describe --tags --always --dirty 2^>NUL') do set "OCGO_VERSION=%%v"
if not defined OCGO_VERSION set "OCGO_VERSION=dev"

if not exist bin mkdir bin
go build -ldflags "-X main.version=%OCGO_VERSION%" -o bin\ocgo.exe .\cmd\ocgo
exit /b %ERRORLEVEL%
