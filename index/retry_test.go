package index

import (
	"errors"
	"math"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/fy0/bluge/index/mergeplan"
	segment "github.com/fy0/bluge/segment"
)

type diskFullDirectory struct {
	Directory
	mode     string
	full     atomic.Bool
	attempts atomic.Uint64
}

func (d *diskFullDirectory) Persist(kind string, id uint64, w WriterTo, closeCh chan struct{}) error {
	_, merging := w.(segment.Merger)
	if (d.mode == "merge" && merging) || (d.mode == "snapshot" && kind == ItemKindSnapshot) ||
		(d.mode == "segment" && kind == ItemKindSegment) {
		d.attempts.Add(1)
		if d.full.Load() {
			return &os.PathError{Op: "write", Path: "injected-full-volume", Err: syscall.ENOSPC}
		}
	}
	return d.Directory.Persist(kind, id, w, closeCh)
}

type retryFailure struct {
	err error
	at  time.Time
}

func openDiskFullWriter(t *testing.T, mode string, initialDelay, maxDelay time.Duration) (*Writer, *diskFullDirectory, <-chan retryFailure) {
	t.Helper()
	dir := &diskFullDirectory{Directory: NewFileSystemDirectory(t.TempDir()), mode: mode}
	cfg := DefaultConfigWithDirectory(func() Directory { return dir }).WithBackgroundRetryBackoff(initialDelay, maxDelay)
	cfg.PersisterNapTimeMSec = 0
	cfg.PersisterNapUnderNumFiles = 1000
	cfg.MergePlanOptions.CalcBudget = func(int64, int64, *mergeplan.Options) int { return 1 }
	failures := make(chan retryFailure, 100)
	cfg.AsyncError = func(err error) {
		select {
		case failures <- retryFailure{err: err, at: time.Now()}:
		default:
		}
	}
	w, err := OpenWriter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	dir.full.Store(true)
	return w, dir, failures
}

func retryTestBatch(id string) *Batch {
	batch := NewBatch()
	batch.Update(testIdentifier(id), &FakeDocument{
		NewFakeField("_id", id, true, false, false),
		NewFakeField("body", "common term", false, false, true),
	})
	return batch
}

func triggerDiskFull(t *testing.T, w *Writer, mode string) {
	t.Helper()
	if err := w.Batch(retryTestBatch("1")); mode != "merge" {
		if !errors.Is(err, syscall.ENOSPC) {
			t.Fatalf("batch error: got %v, want ENOSPC", err)
		}
		return
	} else if err != nil {
		t.Fatal(err)
	}
	if err := w.Batch(retryTestBatch("2")); err != nil {
		t.Fatal(err)
	}
}

func nextRetryFailure(t *testing.T, failures <-chan retryFailure) retryFailure {
	t.Helper()
	select {
	case failure := <-failures:
		if !errors.Is(failure.err, syscall.ENOSPC) {
			t.Fatalf("async error lost disk-full cause: %v", failure.err)
		}
		return failure
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for injected failure")
		return retryFailure{}
	}
}

func closeDiskFullWriter(t *testing.T, w *Writer) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- w.Close() }()
	select {
	case err := <-done:
		if !errors.Is(err, syscall.ENOSPC) {
			t.Fatalf("close error: got %v, want ENOSPC", err)
		}
	case <-time.After(750 * time.Millisecond):
		t.Fatal("Close blocked on background retries")
	}
}

func TestBackgroundRetryCloseDuringBackoff(t *testing.T) {
	for _, mode := range []string{"merge", "snapshot", "segment"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			w, dir, failures := openDiskFullWriter(t, mode, time.Second, 5*time.Minute)
			triggerDiskFull(t, w, mode)
			nextRetryFailure(t, failures)
			closeDiskFullWriter(t, w)
			if got := dir.attempts.Load(); got != 1 {
				t.Fatalf("Close retried failed work: got %d attempts, want 1", got)
			}
		})
	}
}

func waitForRetryRecovery(t *testing.T, w *Writer, mode string, docs uint64) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		r := w.currentSnapshot()
		count, err := r.Count()
		merged := len(r.segment) == 1 && atomic.LoadUint64(&w.stats.LastMergedEpoch) >= r.epoch
		persisted := atomic.LoadUint64(&w.stats.LastPersistedEpoch) >= r.epoch
		_ = r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if count == docs && persisted && (mode != "merge" || merged) {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("background work did not recover after freeing space")
		case <-ticker.C:
		}
	}
}

func TestBackgroundRetryRecoversAndResets(t *testing.T) {
	for _, mode := range []string{"merge", "snapshot", "segment"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			w, dir, failures := openDiskFullWriter(t, mode, 0, 0)
			triggerDiskFull(t, w, mode)
			nextRetryFailure(t, failures)
			attempts := dir.attempts.Load()
			time.Sleep(200 * time.Millisecond)
			if got := dir.attempts.Load(); got != attempts {
				t.Fatalf("retried before backoff: got %d attempts, want %d", got, attempts)
			}
			dir.full.Store(false)
			docs := uint64(1)
			if mode == "merge" {
				docs = 2
			}
			waitForRetryRecovery(t, w, mode, docs)

			// A later, independent failure starts with the initial delay again.
			dir.full.Store(true)
			err := w.Batch(retryTestBatch("3"))
			if mode == "merge" && err != nil {
				t.Fatal(err)
			}
			if mode != "merge" && !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("batch error: got %v, want ENOSPC", err)
			}
			failure := nextRetryFailure(t, failures)
			if !strings.Contains(failure.err.Error(), "retry in 1s") {
				t.Fatalf("retry delay did not reset: %v", failure.err)
			}
			dir.full.Store(false)
			waitForRetryRecovery(t, w, mode, docs+1)
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBackgroundRetryContinuesAtCapAndRecoversWithoutWrites(t *testing.T) {
	for _, mode := range []string{"merge", "snapshot", "segment"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			initialDelay := 40 * time.Millisecond
			maxDelay := 160 * time.Millisecond
			w, dir, failures := openDiskFullWriter(t, mode, initialDelay, maxDelay)
			triggerDiskFull(t, w, mode)
			previous := nextRetryFailure(t, failures)
			for i, minimum := range []time.Duration{
				initialDelay, 2 * initialDelay, maxDelay, maxDelay, maxDelay, maxDelay, maxDelay,
			} {
				failure := nextRetryFailure(t, failures)
				if elapsed := failure.at.Sub(previous.at); elapsed < minimum-10*time.Millisecond {
					t.Fatalf("retry %d after %s, want at least %s", i+1, elapsed, minimum)
				}
				previous = failure
			}
			if !strings.Contains(previous.err.Error(), "retry in 160ms") {
				t.Fatalf("retry interval did not saturate: %v", previous.err)
			}
			if got := dir.attempts.Load(); got != 8 {
				t.Fatalf("attempts: got %d, want 8", got)
			}

			// Freeing space must recover the unchanged work even after more than
			// five retries, without another Batch or a writer reopen.
			dir.full.Store(false)
			docs := uint64(1)
			if mode == "merge" {
				docs = 2
			}
			waitForRetryRecovery(t, w, mode, docs)
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBackgroundRetryDelayPolicy(t *testing.T) {
	for _, test := range []struct {
		name     string
		initial  time.Duration
		maximum  time.Duration
		expected []time.Duration
	}{
		{
			name: "defaults", initial: time.Second, maximum: time.Minute,
			expected: []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
				16 * time.Second, 32 * time.Second, time.Minute, time.Minute},
		},
		{
			name: "non power of two cap", initial: 2 * time.Second, maximum: 5 * time.Second,
			expected: []time.Duration{2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second},
		},
		{
			name: "duration overflow", initial: time.Duration(math.MaxInt64/2 + 1), maximum: time.Duration(math.MaxInt64),
			expected: []time.Duration{time.Duration(math.MaxInt64/2 + 1), time.Duration(math.MaxInt64), time.Duration(math.MaxInt64)},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			retry := newBackgroundRetry(DefaultConfig("").WithBackgroundRetryBackoff(test.initial, test.maximum))
			defer retry.stop()
			for _, want := range test.expected {
				err := retry.fail("test failure", syscall.ENOSPC)
				if !errors.Is(err, syscall.ENOSPC) {
					t.Fatalf("lost cause: %v", err)
				}
				if retry.delay != want {
					t.Fatalf("retry delay: got %s, want %s", retry.delay, want)
				}
			}
			retry.reset()
			_ = retry.fail("test failure", syscall.ENOSPC)
			if retry.delay != test.initial {
				t.Fatalf("reset delay: got %s, want %s", retry.delay, test.initial)
			}
		})
	}
}

func TestBackgroundRetryConfiguration(t *testing.T) {
	for _, test := range []struct {
		name    string
		initial time.Duration
		maximum time.Duration
		invalid bool
	}{
		{name: "zero uses defaults"},
		{name: "custom", initial: time.Second, maximum: 5 * time.Minute},
		{name: "negative initial", initial: -time.Second, invalid: true},
		{name: "negative maximum", maximum: -time.Second, invalid: true},
		{name: "maximum below initial", initial: 2 * time.Second, maximum: time.Second, invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := InMemoryOnlyConfig().WithBackgroundRetryBackoff(test.initial, test.maximum)
			w, err := OpenWriter(cfg)
			if test.invalid {
				if err == nil {
					_ = w.Close()
					t.Fatal("invalid retry delays accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = w.Close() }()
			initial, maximum := test.initial, test.maximum
			if initial == 0 {
				initial = time.Second
			}
			if maximum == 0 {
				maximum = time.Minute
			}
			if w.config.BackgroundRetryInitialDelay != initial || w.config.BackgroundRetryMaxDelay != maximum {
				t.Fatalf("retry configuration: got %s/%s, want %s/%s",
					w.config.BackgroundRetryInitialDelay, w.config.BackgroundRetryMaxDelay, initial, maximum)
			}
		})
	}
}
