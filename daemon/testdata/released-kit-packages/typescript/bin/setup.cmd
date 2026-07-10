@echo off
REM Kit post-acquire hook: TypeScript (foundation) — Windows
REM Runs once after the workarea is acquired and ready.

echo [typescript kit] post_acquire: checking workarea...

IF NOT EXIST node_modules (
  echo [typescript kit] Installing dependencies...
  where pnpm >nul 2>&1
  IF %ERRORLEVEL% == 0 (
    pnpm install --prefer-offline
  ) ELSE (
    npm install
  )
)

echo [typescript kit] post_acquire: done.
