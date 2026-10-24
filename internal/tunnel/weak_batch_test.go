package tunnel

import (
	"bytes"
	"testing"
	"time"
)

func TestWeakDueBatchPreservesTimingOrderAndBounds(t *testing.T) {
	now := time.Now()
	packet := func(b byte, due time.Time) weakPacket { return weakPacket{[]byte{b}, due} }
	for _, tc := range []struct {
		name    string
		times   []time.Time
		limit   int
		want    []byte
		pending byte
		left    int
	}{
		{"all due", []time.Time{now, now}, 16, []byte{1, 2, 3}, 0, 0},
		{
			"future ordering barrier",
			[]time.Time{now.Add(time.Second), now.Add(-time.Second)},
			16,
			[]byte{1},
			2,
			1,
		},
		{"bounded batch", []time.Time{now, now}, 2, []byte{1, 2}, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := make(chan weakPacket, len(tc.times))
			for i, due := range tc.times {
				q <- packet(byte(i+2), due)
			}
			close(q)
			data, pending, merged := dueWeakBatch(packet(1, now), q, now, tc.limit)
			if !bytes.Equal(data, tc.want) || merged != len(tc.want)-1 || len(q) != tc.left {
				t.Fatalf("batch=%v merged=%d left=%d", data, merged, len(q))
			}
			if tc.pending == 0 {
				if pending != nil {
					t.Fatal("unexpected pending segment")
				}
			} else if pending == nil || pending.data[0] != tc.pending || !pending.due.After(now) {
				t.Fatal("future segment bypassed or lost")
			}
		})
	}
	t.Run("segment larger than remaining space", func(t *testing.T) {
		q := make(chan weakPacket, 1)
		q <- weakPacket{[]byte{2, 3}, now}
		data, pending, merged := dueWeakBatch(packet(1, now), q, now, 2)
		if !bytes.Equal(data, []byte{1}) || pending == nil || !bytes.Equal(pending.data, []byte{2, 3}) ||
			merged != 0 {
			t.Fatal("oversize segment was split or lost")
		}
	})
}
