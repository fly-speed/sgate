package tunnel

import (
	"fmt"
	"testing"
	"time"
)

func TestQUICNoisePacerBudgetAndNoCatchup(t *testing.T) {
	for _, rate := range []int{1, 1024, 64 << 10, 10 << 20} {
		t.Run(fmt.Sprint(rate), func(t *testing.T) {
			o, err := (Options{NoiseInterval: time.Millisecond, NoiseMinBytes: rate, NoiseMaxBytes: rate}).defaults()
			if err != nil {
				t.Fatal(err)
			}
			p := quicNoisePacer{}
			start := time.Unix(100, 0)
			now := start
			buckets := [3]int{}
			for now.Before(start.Add(3 * time.Second)) {
				size, wait, err := p.poll(now, o, true)
				if err != nil {
					t.Fatal(err)
				}
				if size > 0 {
					if size > chunkSize {
						t.Fatal("oversized paced frame", size)
					}
					buckets[int(now.Sub(start)/time.Second)] += size
					p.sent(now)
				} else {
					if wait <= 0 {
						t.Fatal("enabled pacer has no wakeup")
					}
					now = now.Add(wait)
				}
			}
			for _, total := range buckets {
				if total > rate+chunkSize {
					t.Fatal("per-second rate exceeded", buckets)
				}
			}
			if rate >= 1024 && buckets[1] < rate*9/10 {
				t.Fatal("steady rate did not approach target", buckets)
			}
			// A long write stall can release only one frame, never a backlog.
			now = now.Add(time.Hour)
			size, _, err := p.poll(now, o, true)
			if err != nil || size <= 0 || size > chunkSize {
				t.Fatal(size, err)
			}
			p.sent(now)
			size, wait, err := p.poll(now, o, true)
			if err != nil || size != 0 || wait <= 0 {
				t.Fatal("stall banked credit", size, wait, err)
			}
			p.poll(now, o, false)
			size, wait, err = p.poll(now.Add(time.Hour), o, true)
			if err != nil || size != 0 || wait <= 0 {
				t.Fatal("pause banked credit", size, wait, err)
			}
		})
	}
}
func TestQUICNoisePacerLiveChanges(t *testing.T) {
	o, _ := (Options{NoiseInterval: time.Millisecond, NoiseMinBytes: 64 << 10, NoiseMaxBytes: 64 << 10}).defaults()
	p := quicNoisePacer{}
	now := time.Unix(100, 0)
	_, wait, _ := p.poll(now, o, true)
	deadline := now.Add(wait)
	// Unrelated changes must keep the original deadline.
	for i := 1; i < 10; i++ {
		o.Logf = func(string, ...any) {}
		_, _, err := p.poll(now.Add(wait*time.Duration(i)/10), o, true)
		if err != nil || p.nextSend != deadline {
			t.Fatal("unrelated change reset pacing", err)
		}
	}
	o.NoiseMinBytes = 1024
	o.NoiseMaxBytes = 1024
	size, wait, err := p.poll(deadline, o, true)
	if err != nil || size != 0 || wait <= 0 || p.size > 1024/50 {
		t.Fatal("rate change retained old credit", size, wait, err)
	}
	o.NoiseInterval = 0
	size, wait, err = p.poll(deadline.Add(time.Hour), o, true)
	if err != nil || size != 0 || wait != 0 {
		t.Fatal("disabled noise scheduled work", size, wait, err)
	}
}
func TestQUICNoisePayloadRate(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprintf("native=%t", native), func(t *testing.T) {
			options := [2]Options{testOptions(), testOptions()}
			stats := [2]*Stats{NewStats(), NewStats()}
			const rate = 64 << 10
			for i := range options {
				options[i].Stats = stats[i]
				options[i].NoiseInterval = time.Millisecond
				options[i].NoiseMinBytes = rate
				options[i].NoiseMaxBytes = rate
			}
			newFixtureWithTransport(t, time.Minute, nil, options[0], options[1], true, native)
			waitFor(t, "both paced noise directions started", func() bool { return stats[0].Snapshot().NoiseTX > 0 && stats[1].Snapshot().NoiseTX > 0 })
			start := time.Now()
			previous := start
			before := [2]uint64{stats[0].Snapshot().NoiseTX, stats[1].Snapshot().NoiseTX}
			last := before
			for sample := 0; sample < 12; sample++ {
				time.Sleep(100 * time.Millisecond)
				now := time.Now()
				for i := range stats {
					current := stats[i].Snapshot().NoiseTX
					max := uint64(float64(rate)*now.Sub(previous).Seconds()) + 2*(rate/50)
					if current-last[i] > max {
						t.Fatalf("noise sent a burst instead of pacing: side=%d bytes=%d cap=%d", i, current-last[i], max)
					}
					last[i] = current
				}
				previous = now
			}
			elapsed := time.Since(start).Seconds()
			for i := range stats {
				n := float64(last[i] - before[i])
				t.Logf("side=%d target=%d B/s measured=%.0f B/s over %.3fs", i, rate, n/elapsed, elapsed)
				if n < float64(rate)*elapsed*0.5 || n > float64(rate)*elapsed+2*(rate/50) {
					t.Fatalf("side=%d payload=%v duration=%v", i, n, elapsed)
				}
			}
		})
	}
}
