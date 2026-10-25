//go:build unix

package master

import (
	"fmt"
	"os"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// acl_master's contract reserves FD 5 for state and 6.. for listeners. Some
// masters also inherit their accepted management sockets into restarted workers.
// Those sockets have no Go owner, and must not keep master_ctl peers alive.
// Go-created sockets use CLOEXEC; preserve them, regular files and pipes.
func closeInheritedSockets(listenerCount int) (int, error) {
	directory, e := os.Open("/proc/self/fd")
	if e != nil {
		directory, e = os.Open("/dev/fd")
	}
	if e != nil {
		return 0, fmt.Errorf("enumerate inherited descriptors: %w", e)
	}
	// /dev/fd entries can disappear while another goroutine closes a pipe.
	// Read names only: ReadDir may stat each entry and fail on a vanished FD.
	names, e := directory.Readdirnames(-1)
	directory.Close()
	if e != nil {
		return 0, fmt.Errorf("enumerate inherited descriptors: %w", e)
	}
	// Prevent an ordinary Go socket creation/fork from crossing the descriptor
	// inspection window on platforms that set CLOEXEC after socket().
	syscall.ForkLock.Lock()
	defer syscall.ForkLock.Unlock()
	closed := 0
	for _, name := range names {
		fd, e := strconv.Atoi(name)
		if e != nil || fd < 3 || (fd >= 5 && fd < 6+listenerCount) {
			continue
		}
		flags, e := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if e == unix.EBADF {
			continue
		}
		if e != nil {
			return closed, fmt.Errorf("inspect inherited descriptor %d: %w", fd, e)
		}
		if flags&unix.FD_CLOEXEC != 0 {
			continue
		}
		if _, e = unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TYPE); e != nil {
			if e == unix.ENOTSOCK || e == unix.EBADF {
				continue
			}
			return closed, fmt.Errorf("inspect inherited socket %d: %w", fd, e)
		}
		if e = unix.Close(fd); e != nil {
			return closed, fmt.Errorf("close inherited socket %d: %w", fd, e)
		}
		closed++
	}
	return closed, nil
}
