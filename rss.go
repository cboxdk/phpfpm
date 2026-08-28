package phpfpm

import (
	"log/slog"

	"github.com/shirou/gopsutil/v3/process"
)

// enrichWorkerRSS fills in each worker's resident memory.
//
// PHP-FPM's status page does not report it. The worker fields it does send are
// pid, state, request counts, timings, the last request's URI and script, and
// "last request memory" — which is PHP's own peak for that one request, not what
// the process is holding now, and is zero for a worker sitting idle.
//
// So the size of a worker has to be read from the operating system, using the
// pid the status page does give us. Without this the CurrentRSS field is
// permanently zero, which is what it was before: declared, serialised, and never
// assigned.
//
// Failures are silent per worker. A worker that exited between the status
// response and this read is the common case, not an error, and one that has gone
// is correctly worth nothing.
func enrichWorkerRSS(pool *Pool, log *slog.Logger) {
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

		pool.Processes[i].CurrentRSS = int64(mem.RSS)
	}

	if missing > 0 {
		// Worth a line at debug: a scrape where MOST workers could not be read
		// usually means the caller lacks permission to inspect another user's
		// processes, which silently produces a pool that looks free.
		log.Debug("Some workers' memory could not be read",
			"pool", pool.Name, "missing", missing, "total", len(pool.Processes))
	}
}
