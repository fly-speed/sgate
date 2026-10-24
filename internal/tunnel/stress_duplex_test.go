package tunnel

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	mathrand "math/rand/v2"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// PCG consumes all seed bits. The legacy Source reduces seeds modulo 2^31-1,
// making client/round fields in the upper bits collide with other stream IDs.
type stressByteStream struct {
	rng   *mathrand.Rand
	block [8]byte
	used  int
}

func newStressByteStream(seed uint64) *stressByteStream {
	return &stressByteStream{rng: mathrand.New(mathrand.NewPCG(seed, seed^0xa0761d6478bd642f)), used: 8}
}
func (r *stressByteStream) Read(p []byte) (int, error) {
	size := len(p)
	for len(p) > 0 {
		if r.used == 8 {
			binary.LittleEndian.PutUint64(r.block[:], r.rng.Uint64())
			r.used = 0
		}
		n := copy(p, r.block[r.used:])
		r.used += n
		p = p[n:]
	}
	return size, nil
}
func TestStressByteStreamPreservesHighSeedBits(t *testing.T) {
	a, b := make([]byte, 1024), make([]byte, 1024)
	// These two seeds generated identical streams with math/rand.NewSource.
	_, _ = newStressByteStream(uint64(1)<<48 | 2).Read(a)
	_, _ = newStressByteStream(uint64(1)<<48 | uint64(1)<<32).Read(b)
	if bytes.Equal(a, b) {
		t.Fatal("different clients generated identical verifier data")
	}
	split := make([]byte, len(a))
	r := newStressByteStream(uint64(1)<<48 | 2)
	_, _ = r.Read(split[:3])
	_, _ = r.Read(split[3:])
	if !bytes.Equal(a, split) {
		t.Fatal("verifier byte stream depends on read boundaries")
	}
}

type stressRecordInput struct {
	net.Conn
	data *bytes.Reader
}

func (c stressRecordInput) Read(p []byte) (int, error) { return c.data.Read(p) }

func TestStressDuplexVerifierRejectsMissingAndRepeatedData(t *testing.T) {
	const size = 64
	r := newStressByteStream(42)
	var records [3][]byte
	for i := range records {
		records[i] = make([]byte, 9)
		binary.BigEndian.PutUint64(records[i][1:], uint64(i))
		if i == 2 {
			records[i][0] = 1
		} else {
			p := make([]byte, size)
			r.Read(p)
			records[i] = append(records[i], p...)
		}
	}
	join := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	valid := join(records[0], records[1], records[2])
	corrupt := append([]byte(nil), valid...)
	corrupt[9] ^= 1
	for name, data := range map[string][]byte{
		"valid":             valid,
		"missing record":    join(records[1], records[2]),
		"repeated record":   join(records[0], records[0], records[1], records[2]),
		"corrupt payload":   corrupt,
		"missing terminal":  join(records[0], records[1]),
		"truncated payload": records[0][:10],
		"extra bytes":       append(append([]byte(nil), valid...), 1),
	} {
		t.Run(name, func(t *testing.T) {
			var total atomic.Int64
			err := stressReadRecords(stressRecordInput{data: bytes.NewReader(data)}, 42, size, &total)
			if name == "valid" {
				if err != nil || total.Load() != 2*size {
					t.Fatalf("valid stream: bytes=%d err=%v", total.Load(), err)
				}
			} else if err == nil {
				t.Fatal("verifier accepted missing, repeated or corrupt data")
			}
		})
	}
}

// Independent writers use different reproducible random byte streams. Sequence
// headers and a required terminal record detect duplicates, gaps and lost tails,
// including complete missing records. EOF alone is not considered success.
func stressWriteRecords(c net.Conn, seed uint64, end time.Time, size int, total *atomic.Int64, interval time.Duration) error {
	r := newStressByteStream(seed)
	p := make([]byte, size)
	var header [9]byte
	for seq := uint64(0); ; seq++ {
		binary.BigEndian.PutUint64(header[1:], seq)
		if seq > 0 && !time.Now().Before(end) {
			header[0] = 1
			if _, err := io.Copy(c, bytes.NewReader(header[:])); err != nil {
				return err
			}
			return c.(interface{ CloseWrite() error }).CloseWrite()
		}
		r.Read(p)
		if _, err := io.Copy(c, bytes.NewReader(header[:])); err != nil {
			return err
		}
		if _, err := io.Copy(c, bytes.NewReader(p)); err != nil {
			return err
		}
		total.Add(int64(size))
		time.Sleep(interval)
	}
}

func stressReadRecords(c net.Conn, seed uint64, size int, total *atomic.Int64) error {
	r := newStressByteStream(seed)
	got, want := make([]byte, size), make([]byte, size)
	var header [9]byte
	for seq := uint64(0); ; seq++ {
		if _, err := io.ReadFull(c, header[:]); err != nil {
			return fmt.Errorf("missing record %d or terminal marker: %w", seq, err)
		}
		if binary.BigEndian.Uint64(header[1:]) != seq {
			return fmt.Errorf("record lost or duplicated: expected %d got %d", seq, binary.BigEndian.Uint64(header[1:]))
		}
		if header[0] == 1 {
			var extra [1]byte
			if n, err := c.Read(extra[:]); n != 0 || err != io.EOF {
				return fmt.Errorf("extra bytes or missing EOF after terminal marker: n=%d err=%v", n, err)
			}
			return nil
		}
		if header[0] != 0 {
			return fmt.Errorf("corrupt record type: %d", header[0])
		}
		if _, err := io.ReadFull(c, got); err != nil {
			return err
		}
		r.Read(want)
		if !bytes.Equal(got, want) {
			return fmt.Errorf("record %d payload lost, duplicated or corrupted", seq)
		}
		total.Add(int64(size))
	}
}

func stressDuplex(c net.Conn, seed uint64, end time.Time, size int, backend bool, sent, received *atomic.Int64, interval time.Duration) error {
	txSeed, rxSeed := seed, seed^0xa0761d6478bd642f
	if backend {
		txSeed, rxSeed = rxSeed, txSeed
	}
	done := make(chan error, 1)
	go func() { done <- stressWriteRecords(c, txSeed, end, size, sent, interval) }()
	err := stressReadRecords(c, rxSeed, size, received)
	if err != nil {
		c.Close() // Unblock the independent writer before joining it.
	}
	if writeErr := <-done; err == nil {
		err = writeErr
	}
	return err
}
