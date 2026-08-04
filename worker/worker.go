// Package worker implements the donmai worker protocol: registration
// with the platform, work polling, heartbeat reporting, and multi-worker
// fleet process management.
//
// This package is public so downstream projects can
// import it for fleet lifecycle commands that route through the platform
// API proxy.
package worker
