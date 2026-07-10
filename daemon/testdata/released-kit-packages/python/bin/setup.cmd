@echo off
REM Kit post-acquire hook: Python (Windows)
REM Runs once after the workarea is acquired and ready.

echo [python kit] post_acquire: installing dependencies...

where uv >nul 2>&1
IF %ERRORLEVEL% == 0 (
  IF EXIST pyproject.toml (
    uv sync
    goto :done
  )
)

IF EXIST requirements.txt (
  py -m venv .venv
  call .venv\Scripts\activate.bat
  pip install -r requirements.txt
) ELSE (
  echo [python kit] No recognized dependency file. Skipping install.
)

:done
echo [python kit] post_acquire: done.
