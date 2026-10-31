package master

import (
	"encoding/binary"
	"io"
	"net"
	"os"
	"testing"
)

func TestNotifyStartedNativeProtocolAndOctalGeneration(t *testing.T) {
	t.Setenv("GENERATION", "17")
	child, parent := net.Pipe()
	defer child.Close()
	defer parent.Close()
	done := make(chan error, 1)
	go func() { done <- NotifyStarted(child) }()
	var packet [12]byte
	if _, err := io.ReadFull(parent, packet[:]); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for i, want := range []uint32{uint32(os.Getpid()), 15, 4} {
		if got := binary.NativeEndian.Uint32(packet[i*4 : i*4+4]); got != want {
			t.Fatalf("word %d = %d, want %d", i, got, want)
		}
	}
}

func TestNotifyStartedRejectsInvalidGeneration(t *testing.T) {
	for _, value := range []string{"", "8", "-1", "40000000000"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("GENERATION", value)
			child, parent := net.Pipe()
			defer child.Close()
			defer parent.Close()
			if err := NotifyStarted(child); err == nil {
				t.Fatal("invalid generation accepted")
			}
		})
	}
}
