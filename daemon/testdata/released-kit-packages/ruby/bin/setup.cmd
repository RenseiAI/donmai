@echo off
REM Kit post-acquire hook: Ruby (Windows)
REM Runs once after the workarea is acquired and ready.

echo [ruby kit] post_acquire: installing gems...

where bundle >nul 2>&1
IF %ERRORLEVEL% == 0 (
  IF EXIST Gemfile (
    bundle install
  )
) ELSE (
  echo [ruby kit] bundler not found. Skipping install.
)

echo [ruby kit] post_acquire: done.
