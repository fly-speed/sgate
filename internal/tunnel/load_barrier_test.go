package tunnel

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func waitStressLoadBarrier(ctx context.Context, prefix string, round, streams int, budget time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ready := fmt.Sprintf("%s-round-%d.ready", prefix, round)
	if err := os.WriteFile(ready+".tmp", []byte(fmt.Sprint(streams)), 0644); err != nil {
		return err
	}
	if err := os.Rename(ready+".tmp", ready); err != nil {
		return err
	}
	start := filepath.Join(filepath.Dir(prefix), fmt.Sprintf("round-%d.start", round))
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("round %d full-load barrier: %w", round, err)
		}
		if _, err := os.Stat(start); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
}

func TestStressLoadBarrierWaitsAndCancels(t *testing.T) {
	for _, release := range []bool{true, false} {
		t.Run(fmt.Sprint(release), func(t *testing.T) {
			dir := t.TempDir()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- waitStressLoadBarrier(ctx, filepath.Join(dir, "case"), 1, 512, time.Second) }()
			waitFor(
				t,
				"ready marker",
				func() bool {
					_, err := os.Stat(filepath.Join(dir, "case-round-1.ready"))
					return err == nil
				},
			)
			select {
			case <-done:
				t.Fatal("started before release")
			default:
			}
			if release {
				if err := os.WriteFile(filepath.Join(dir, "round-1.start"), nil, 0644); err != nil {
					t.Fatal(err)
				}
			} else {
				cancel()
			}
			select {
			case err := <-done:
				if (err == nil) != release {
					t.Fatalf("release=%t err=%v", release, err)
				}
			case <-time.After(time.Second):
				t.Fatal("barrier leaked waiter")
			}
		})
	}
}
