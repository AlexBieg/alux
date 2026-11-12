package vault

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/alexbieg/alux/internal/paths"
)

// Notes are small and filing is occasional, so a waiter is cheaper than losing a write. Past the
// wait, take the lock: the holder has almost certainly died mid-call.
const (
	lockWait  = 60 * time.Second
	lockStale = 5 * time.Minute
	lockPoll  = 250 * time.Millisecond
)

func lockPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, paths.Cache, "vault-write.lock")
}

func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	// Signal 0 tests for existence. EPERM means the process is there but owned by someone else.
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// staleLock reports whether the lock on disk can be taken from its holder.
func staleLock(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		// The holder released it between our failed create and this read.
		return true
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 {
		// A lock whose holder cannot be identified is treated as held, not free. Reading it
		// the other way is how several writers get in at once: any window where the file
		// exists without its contents looks exactly like an abandoned lock. claim() closes
		// that window, and this makes a lock corrupted by anything else wait out the deadline
		// instead of being seized immediately.
		return false
	}
	pid, _ := strconv.Atoi(fields[0])
	started, _ := strconv.ParseInt(fields[1], 10, 64)
	return !alive(pid) || time.Since(time.UnixMilli(started)) > lockStale
}

// claim takes the lock, content and all, in one atomic step.
//
// The obvious version — create the file exclusively, then write the pid into it — has a window
// where the lock exists but is empty, and a waiter reading it in that window sees an
// unparseable holder, concludes the lock is stale and takes it. Several writers then run at
// once, which is the whole thing the lock exists to prevent. os.Link closes the window: the
// content is already in the temporary file, and linking it into place fails if the name is
// taken.
func claim(path string) error {
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	content := fmt.Sprintf("%d %d", os.Getpid(), time.Now().UnixMilli())
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return err
	}
	defer os.Remove(tmp)
	return os.Link(tmp, path)
}

// WithWriteLock runs job as the only vault writer across every alux on this machine.
//
// Two sessions filing at once means duplicate notes from a concurrent search-then-create. This
// is one writer at a time without a daemon to own the queue.
func WithWriteLock(ctx context.Context, job func() error) error {
	path := lockPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	deadline := time.Now().Add(lockWait)
	for {
		err := claim(path)
		if err == nil {
			break
		}
		// Only a name already taken means someone holds the lock. Any other failure — a full
		// disk, a read-only cache directory, a filesystem without hardlinks — is permanent, and
		// retrying it spins: staleLock reads a file that was never created, calls it abandoned,
		// removes nothing and loops without ever reaching the wait below. That was an
		// uninterruptible busy loop at full CPU, immune to ctx.
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("cannot take the vault write lock at %s: %w", path, err)
		}
		if time.Now().After(deadline) || staleLock(path) {
			_ = os.Remove(path)
			// Checked even on the seize path, so a caller that gave up is not held by a lock
			// that keeps being retaken under it.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(lockPoll):
		}
	}

	defer os.Remove(path)
	return job()
}
