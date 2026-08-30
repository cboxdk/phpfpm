package phpfpm

import (
	"log/slog"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v3/process"
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

	enrichWorkerRSS(pool, nil, nil)

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
	enrichWorkerRSS(pool, nil, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

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
	enrichWorkerRSS(pool, nil, nil)
}

// TestSubtreeRSSCountsAChild is the whole point of the second number: a worker
// that spawns a process — an ffmpeg, here stood in for by `sleep` — must have a
// subtree RSS strictly larger than its own, because the child is a separate pid
// whose memory the worker's own RSS does not include.
func TestSubtreeRSSCountsAChild(t *testing.T) {
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Skipf("cannot start a child process: %v", err)
	}
	defer func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	}()

	// Built AFTER the child is started, so the snapshot contains it.
	snap := snapshotProcesses()
	if snap == nil {
		t.Skip("the process table could not be read on this host")
	}

	pool := &Pool{Name: "self", Processes: []PoolProcess{{PID: os.Getpid()}}}
	enrichWorkerRSS(pool, snap, nil)

	own := pool.Processes[0].CurrentRSS
	subtree := pool.Processes[0].SubtreeRSS
	if own <= 0 {
		t.Fatal("own RSS was not read")
	}
	if subtree <= own {
		t.Errorf("subtree RSS %d did not exceed own RSS %d, but a live child was spawned; "+
			"the child's memory is being lost, which is exactly the ffmpeg case", subtree, own)
	}
	t.Logf("own %d, subtree %d, children ~%d bytes", own, subtree, subtree-own)
}

// TestSubtreeRSSNeverBelowOwn: the two reads are a fraction of a second apart,
// and "children" is their difference — it must never be reported negative, even
// with no snapshot (subtree stays zero, meaning "not measured", not "-own").
func TestSubtreeRSSNeverBelowOwn(t *testing.T) {
	pool := &Pool{Name: "self", Processes: []PoolProcess{{PID: os.Getpid()}}}

	enrichWorkerRSS(pool, snapshotProcesses(), nil)
	if s := pool.Processes[0]; s.SubtreeRSS != 0 && s.SubtreeRSS < s.CurrentRSS {
		t.Errorf("subtree RSS %d is below own RSS %d", s.SubtreeRSS, s.CurrentRSS)
	}
}

// TestSubtreeRSSTerminatesOnACycle: the process table should never contain a
// parent cycle, but a read racing with fork/reparent could momentarily present
// one. The walk must terminate rather than recurse until the stack is gone.
func TestSubtreeRSSTerminatesOnACycle(t *testing.T) {
	self, err := process.NewProcess(int32(os.Getpid()))
	if err != nil {
		t.Fatalf("cannot open self: %v", err)
	}

	snap := &processSnapshot{
		children: map[int32][]int32{1: {2}, 2: {1}}, // 1 <-> 2, a cycle
		byPID:    map[int32]*process.Process{1: self, 2: self},
	}

	done := make(chan int64, 1)
	go func() { done <- snap.subtreeRSS(1) }()
	select {
	case <-done:
		// Terminated, which is all this asserts.
	case <-time.After(2 * time.Second):
		t.Fatal("subtreeRSS did not terminate on a cyclic process table")
	}
}
