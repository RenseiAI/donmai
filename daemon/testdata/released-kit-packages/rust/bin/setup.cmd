@echo off
REM Kit post-acquire hook: Rust (Windows)
REM Runs once after the workarea is acquired and ready.

echo [rust kit] post_acquire: fetching crate dependencies...

where cargo >nul 2>&1
IF %ERRORLEVEL% == 0 (
  IF EXIST Cargo.toml (
    cargo fetch
  )
) ELSE (
  echo [rust kit] WARNING: cargo not found. Skipping fetch.
)

echo [rust kit] post_acquire: done.
