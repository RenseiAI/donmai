// Package attachwirev2 implements the additive host-carrier successor to the
// frozen interactive-attach-v1 protocol.
//
// V2 reuses v1's binary frame and payload bytes by importing attachwire; it does
// not add a type or control to v1's closed registries. This package owns the
// distinct negotiation token and the four v2-only controls.
package attachwirev2
