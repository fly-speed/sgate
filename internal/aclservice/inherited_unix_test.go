//go:build unix

package aclservice

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestInheritedSocketCleanupSubprocess(t *testing.T) {
	var files []*os.File
	add := func(fd int) { unix.CloseOnExec(fd); files = append(files, os.NewFile(uintptr(fd), "inherited-test")) }
	regular, e := os.CreateTemp(t.TempDir(), "regular")
	if e != nil {
		t.Fatal(e)
	}
	files = append(files, regular)
	read, write, e := os.Pipe()
	if e != nil {
		t.Fatal(e)
	}
	files = append(files, read)
	defer write.Close()
	state, e := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if e != nil {
		t.Fatal(e)
	}
	add(state[0])
	unix.CloseOnExec(state[1])
	defer unix.Close(state[1])
	for i := 0; i < 3; i++ {
		fd, e := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		if e != nil {
			t.Fatal(e)
		}
		add(fd)
	}
	// Child descriptors: regular=3, pipe=4, state=5, listener=6,
	// unwanted inherited sockets=7,8. The parent's originals stay untouched.
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	cmd := exec.Command(os.Args[0], "-test.run=^TestInheritedSocketCleanupChild$")
	cmd.Env = append(os.Environ(), "SGATE_INHERITED_SOCKET_TEST=1")
	cmd.ExtraFiles = files
	out, e := cmd.CombinedOutput()
	if e != nil || !strings.Contains(string(out), "PASS") {
		t.Fatalf("cleanup child failed: %v\n%s", e, out)
	}
}
func TestInheritedSocketCleanupChild(t *testing.T) {
	if os.Getenv("SGATE_INHERITED_SOCKET_TEST") != "1" {
		t.Skip("subprocess only")
	}
	owned, e := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if e != nil {
		t.Fatal(e)
	}
	unix.CloseOnExec(owned)
	defer unix.Close(owned)
	count, e := closeInheritedSockets(1)
	if e != nil || count != 2 {
		t.Fatalf("closed=%d error=%v", count, e)
	}
	for _, fd := range []int{3, 4, 5, 6, owned} {
		if _, e := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); e != nil {
			t.Fatalf("owned/reserved descriptor %d closed: %v", fd, e)
		}
	}
	for _, fd := range []int{7, 8} {
		if _, e := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); e != unix.EBADF {
			t.Fatalf("inherited socket %d retained: %v", fd, e)
		}
	}
	count, e = closeInheritedSockets(1)
	if e != nil || count != 0 {
		t.Fatalf("repeat cleanup: %d %v", count, e)
	}
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			select {
			case <-done:
				return
			default:
			}
			read, write, e := os.Pipe()
			if e == nil {
				read.Close()
				write.Close()
			}
		}
	}()
	defer func() { close(done); <-stopped }()
	for i := 0; i < 100; i++ {
		count, e = closeInheritedSockets(1)
		if e != nil || count != 0 {
			t.Fatalf("descriptor churn: %d %v", count, e)
		}
	}
}
