// Package worker implements the AgentFactory worker protocol: registration
// with the platform, work polling, heartbeat reporting, and multi-worker
// fleet process management.
//
// This package is public so downstream projects (e.g. rensei-tui) can
// import it for fleet lifecycle commands that route through the platform
// API proxy.
package worker
