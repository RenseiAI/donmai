@echo off
REM Kit post-acquire hook: Go (Windows)
REM Runs once after the workarea is acquired and ready.

echo [go kit] post_acquire: downloading module dependencies...

where go >nul 2>&1
IF %ERRORLEVEL% == 0 (
  IF EXIST go.mod (
    go mod download
  )
) ELSE (
  echo [go kit] WARNING: go toolchain not found. Skipping download.
)

echo [go kit] post_acquire: done.
