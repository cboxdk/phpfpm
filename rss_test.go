package phpfpm

import (
	"log/slog"
	"os"
	"testing"
)

// TestEnrichWorkerRSSReadsRealMemory covers the gap that made the whole field
// useless: PHP-FPM's status page does not report worker memory, so CurrentRSS
// was declared, serialised, and never assigned — permanently zero.
//
// The test process is a stand-in for a worker: it definitely exists and it
// definitely occupies memory.
func TestEnrichWorkerRSSReadsRealMemory(t *testing.T) {
	pool := &Pool{
		Name: "self",
		Processes: []PoolProcess{
			{PID: os.Getpid()},
		},
	}

	enrichWorkerRSS(pool, nil)

	if pool.Processes[0].CurrentRSS <= 0 {
		t.Fatal("a live process reported no resident memory; " +
			"every capacity decision downstream turns on this number")
	}
	t.Logf("own RSS: %d bytes", pool.Processes[0].CurrentRSS)
}

// TestEnrichWorkerRSSToleratesDeadWorkers: a worker that exited between the
// status response and this read is the common case on a busy pool, not an error.
func TestEnrichWorkerRSSToleratesDeadWorkers(t *testing.T) {
	pool := &Pool{
		Name: "mixed",
		Processes: []PoolProcess{
			{PID: os.Getpid()},
			{PID: 999999}, // gone
			{PID: 0},      // never reported
			{PID: -1},     // nonsense
		},
	}

	// Must not panic, and must not lose the reading it could take.
	enrichWorkerRSS(pool, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	if pool.Processes[0].CurrentRSS <= 0 {
		t.Error("the live worker's memory was lost because its neighbours were dead")
	}
	for i, p := range pool.Processes[1:] {
		if p.CurrentRSS != 0 {
			t.Errorf("worker %d (pid %d) reported %d bytes", i+1, p.PID, p.CurrentRSS)
		}
	}
}

// TestEnrichWorkerRSSAcceptsANilLogger, like every other entry point here.
func TestEnrichWorkerRSSAcceptsANilLogger(t *testing.T) {
	pool := &Pool{Processes: []PoolProcess{{PID: 999999}}}
	enrichWorkerRSS(pool, nil)
}
