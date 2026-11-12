package attach

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// refuseSessionDescendants turns away any process an alux session started, this one or another,
// since an agent's shells share the user's uid and, once escalated, the sandbox's reach. sessions is
// the registry directory, where every session has a <pid>.json. A process that double-forks onto
// launchd still gets past it.
func refuseSessionDescendants(sessions string) func(pid int) error {
	return func(pid int) error {
		self := os.Getpid()
		if pid == self {
			return fmt.Errorf("process %d is this session", pid)
		}
		for range 64 {
			parent, err := parentPID(pid)
			if err != nil {
				return err
			}
			if parent <= 1 {
				return nil
			}
			if parent == self || registered(sessions, parent) {
				return fmt.Errorf("process %d was started by alux session %d", pid, parent)
			}
			pid = parent
		}
		return fmt.Errorf("process %d has too deep an ancestry to check", pid)
	}
}

func registered(sessions string, pid int) bool {
	_, err := os.Stat(filepath.Join(sessions, strconv.Itoa(pid)+".json"))
	return err == nil
}
