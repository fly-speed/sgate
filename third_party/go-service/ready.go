package master

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"time"
)

// NotifyStarted acknowledges readiness on the owned framework status channel.
// This is independent of master_status_notify (busy/idle accounting). The C
// protocol uses three native-endian 32-bit integers; GENERATION is octal.
func NotifyStarted(state net.Conn) error {
	generation, err := strconv.ParseUint(os.Getenv("GENERATION"), 8, 32)
	if err != nil {
		return fmt.Errorf("invalid acl_master GENERATION: %w", err)
	}
	var message [12]byte
	binary.NativeEndian.PutUint32(message[0:4], uint32(os.Getpid()))
	binary.NativeEndian.PutUint32(message[4:8], uint32(generation))
	binary.NativeEndian.PutUint32(message[8:12], 4) // ACL_MASTER_STAT_START_OK
	if err = state.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	defer state.SetWriteDeadline(time.Time{})
	n, err := state.Write(message[:])
	if err == nil && n != len(message) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return fmt.Errorf("acl_master startup acknowledgement: %w", err)
	}
	return nil
}
