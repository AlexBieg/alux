package attach

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func peerPID(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var serr error
	if err := raw.Control(func(fd uintptr) {
		cred, serr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if serr != nil {
		return 0, serr
	}
	return int(cred.Pid), nil
}

// parentPID reads the fourth field of /proc/<pid>/stat, counted after the parenthesised command name.
func parentPID(pid int) (int, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	_, rest, ok := strings.Cut(string(data), ") ")
	fields := strings.Fields(rest)
	if !ok || len(fields) < 2 {
		return 0, fmt.Errorf("unreadable /proc/%d/stat", pid)
	}
	return strconv.Atoi(fields[1])
}
