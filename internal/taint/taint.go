// Package taint holds the one-way sensitivity latch: once sensitive data has been read in this process, nothing may
// leave it, and the side about to make a network request can ask without knowing what a vault is.
//
// It is its own package rather than a corner of the vault's because the vault pulls in a SQLite
// driver, and the caller about to make a network request must not take that dependency on to ask
// a yes/no question.
//
// The latch is process-wide, which is coarser than the per-conversation rule the vault states:
// /clear does not untaint, and a background subagent's read latches the session. That is the
// conservative direction, and the only one a process-wide flag can be wrong in.
package taint

import "sync/atomic"

var tainted atomic.Bool

// Set marks the process tainted. There is no exported way back: a latch that can be cleared is a
// latch something will clear.
func Set() { tainted.Store(true) }

// Tainted reports whether sensitive data has been read in this process.
func Tainted() bool { return tainted.Load() }

// ResetForTest clears the latch. Exported only because the latch is process-wide and the tests
// that set it live in other packages, where they would otherwise pollute one another.
func ResetForTest() { tainted.Store(false) }
