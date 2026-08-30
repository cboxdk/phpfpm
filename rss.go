package phpfpm

import (
	"log/slog"

	"github.com/shirou/gopsutil/v3/process"
)

// A php-fpm worker's memory is two numbers, not one.
//
// The first is the worker process itself: its own resident set, which PHP-FPM's
// status page does not report and which has to be read from the operating system
// using the pid the page does give us. The second is everything the worker
// SPAWNED — an ffmpeg, an imagemagick, anything reached through exec/proc_open —
// each a separate pid with its own resident set that the status page knows
// nothing about and that the worker's own RSS does not include.
//
// The second number is the one that turns a memory autotuner into an OOM. The
// budget it sizes against (a cgroup limit, or the machine) accounts every
// process, children included; sizing from worker-own-RSS alone prices the pool
// as if the ffmpeg were free, hands out a max_children that fits on paper, and
// blows the limit the moment the workers shell out. So both are measured.

// processSnapshot is the host's process table at one instant: which process is
// whose parent, and a handle to read each one's memory. It is built once per
// scrape round and shared across pools — both so every pool is measured against
// the same instant, and so the walk of /proc is paid for once rather than once
// per pool on a host with many.
type processSnapshot struct {
	children map[int32][]int32
	byPID    map[int32]*process.Process
}

// snapshotProcesses reads the process table once.
//
// A nil return is valid and makes subtree enrichment a no-op: a host where the
// table cannot be read — no permission, or no /proc at all — still gets each
// worker's own RSS from the direct read below, and simply no subtree.
func snapshotProcesses() *processSnapshot {
	all, err := process.Processes()
	if err != nil {
		return nil
	}

	snap := &processSnapshot{
		children: make(map[int32][]int32, len(all)),
		byPID:    make(map[int32]*process.Process, len(all)),
	}
	for _, p := range all {
		snap.byPID[p.Pid] = p

		// One /proc/<pid>/stat read per process. A process that exited between
		// the listing and here is skipped rather than treated as an error — the
		// common case on a busy host, not a fault.
		ppid, err := p.Ppid()
		if err != nil {
			continue
		}
		snap.children[ppid] = append(snap.children[ppid], p.Pid)
	}

	return snap
}

// rssOf reads one process's resident memory, or 0 if it cannot be read.
func (s *processSnapshot) rssOf(pid int32) int64 {
	p := s.byPID[pid]
	if p == nil {
		return 0
	}

	mem, err := p.MemoryInfo()
	if err != nil || mem == nil {
		return 0
	}

	return int64(mem.RSS)
}

// subtreeRSS sums the resident memory of pid and every process descended from
// it — the worker and everything it spawned.
//
// `seen` and the depth bound guard against a cycle the process table should
// never contain but a read racing with fork/reparent could momentarily present:
// a self-referential or looping parent link would otherwise recurse forever.
func (s *processSnapshot) subtreeRSS(pid int32) int64 {
	return s.subtreeRSSBounded(pid, make(map[int32]bool), 0)
}

func (s *processSnapshot) subtreeRSSBounded(pid int32, seen map[int32]bool, depth int) int64 {
	if seen[pid] || depth > 64 {
		return 0
	}
	seen[pid] = true

	total := s.rssOf(pid)
	for _, child := range s.children[pid] {
		total += s.subtreeRSSBounded(child, seen, depth+1)
	}

	return total
}

// enrichWorkerRSS fills in each worker's own resident memory, and — when a
// process snapshot is available — the resident memory of its whole subtree.
//
// The own read is done directly and always, because it is the number sizing has
// always depended on and must not regress if the snapshot is missing or a worker
// appeared after it was taken. The subtree is layered on top from the snapshot,
// clamped so it can never come out below the worker's own RSS (the two are read
// a fraction of a second apart, and "children" is their difference — it must not
// go negative because of that skew).
//
// Failures are silent per worker. A worker that exited between the status
// response and this read is the common case, not an error, and one that has gone
// is correctly worth nothing.
func enrichWorkerRSS(pool *Pool, snap *processSnapshot, log *slog.Logger) {
	log = logOrDiscard(log)

	var missing int

	for i := range pool.Processes {
		pid := pool.Processes[i].PID
		if pid <= 0 {
			continue
		}

		proc, err := process.NewProcess(int32(pid))
		if err != nil {
			missing++

			continue
		}

		mem, err := proc.MemoryInfo()
		if err != nil || mem == nil {
			missing++

			continue
		}

		own := int64(mem.RSS)
		pool.Processes[i].CurrentRSS = own

		if snap != nil {
			subtree := snap.subtreeRSS(int32(pid))
			if subtree < own {
				subtree = own
			}
			pool.Processes[i].SubtreeRSS = subtree
		}
	}

	switch {
	case missing == len(pool.Processes) && missing > 0:
		// EVERY worker unreadable is the dangerous case: a non-empty pool that
		// reports zero memory looks free to a consumer that sizes pools, which is
		// the most expensive possible misreading. The usual cause is /proc mounted
		// hidepid while this runs as a user that cannot see the workers. Loud.
		log.Warn("A pool's memory could not be read for ANY of its workers; it will "+
			"look free to anything sizing it. Usually /proc hidepid or a permissions "+
			"mismatch — run where the workers' /proc entries are readable.",
			"pool", pool.Name, "workers", len(pool.Processes))
	case missing > 0:
		log.Debug("Some workers' memory could not be read",
			"pool", pool.Name, "missing", missing, "total", len(pool.Processes))
	}
}
