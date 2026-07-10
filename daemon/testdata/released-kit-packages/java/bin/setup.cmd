@echo off
REM Kit post-acquire hook: Java (Windows)
REM Runs once after the workarea is acquired and ready.

echo [java kit] post_acquire: resolving dependencies...

IF EXIST pom.xml (
  IF EXIST mvnw.cmd (
    call mvnw.cmd -B -q dependency:go-offline
  ) ELSE (
    where mvn >nul 2>&1 && mvn -B -q dependency:go-offline
  )
) ELSE (
  IF EXIST gradlew.bat (
    call gradlew.bat --no-daemon dependencies
  )
)

echo [java kit] post_acquire: done.
