package phpfpm

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cboxdk/fcgx"
)

// serverSoftware is what PHP-FPM records for requests this library makes. It
// shows up in the pool's access log and in the status page's own process list,
// so it names the library rather than whichever tool is calling it.
const serverSoftware = "cboxdk/phpfpm"

type PoolProcess struct {
	PID               int     `json:"pid"`
	State             string  `json:"state"`
	StartTime         int64   `json:"start time"`
	StartSince        int64   `json:"start since"`
	Requests          int64   `json:"requests"`
	RequestDuration   int64   `json:"request duration"`
	RequestMethod     string  `json:"request method"`
	RequestURI        string  `json:"request uri"`
	ContentLength     int64   `json:"content length"`
	User              string  `json:"user"`
	Script            string  `json:"script"`
	LastRequestCPU    float64 `json:"last request cpu"`
	LastRequestMemory float64 `json:"last request memory"`
	// CurrentRSS is the worker's resident memory, in bytes.
	//
	// Not part of PHP-FPM's status output — it is filled in from the operating
	// system using PID, because the size of a worker is the number any capacity
	// decision turns on and the status page does not carry it. Zero means the
	// worker could not be read, usually because it exited between the status
	// response and the lookup.
	CurrentRSS int64 `json:"current_rss"`

	// SubtreeRSS is the resident memory of the worker AND every process it
	// spawned — the ffmpeg or imagemagick a request shelled out to, each a
	// separate pid the status page and CurrentRSS both miss. It is always at
	// least CurrentRSS; the difference is what the children cost. Zero means it
	// was not measured (no process snapshot this scrape), which is distinct from
	// a measured subtree that equals CurrentRSS because nothing was spawned.
	//
	// Point-in-time: a child that lived and died between two scrapes is not in
	// it, and one whose worker exited first has reparented away from the subtree.
	// It is the per-worker view; a cgroup's own high-water mark, where there is a
	// cgroup, catches the transients this cannot.
	SubtreeRSS int64 `json:"subtree_rss"`
}

type Pool struct {
	Address             string            `json:"address"`
	Path                string            `json:"path"`
	Name                string            `json:"pool"`
	ProcessManager      string            `json:"process manager"`
	StartTime           int64             `json:"start time"`
	StartSince          int64             `json:"start since"`
	AcceptedConnections int64             `json:"accepted conn"`
	ListenQueue         int64             `json:"listen queue"`
	MaxListenQueue      int64             `json:"max listen queue"`
	ListenQueueLength   int64             `json:"listen queue len"`
	IdleProcesses       int64             `json:"idle processes"`
	ActiveProcesses     int64             `json:"active processes"`
	TotalProcesses      int64             `json:"total processes"`
	MaxActiveProcesses  int64             `json:"max active processes"`
	MaxChildrenReached  int64             `json:"max children reached"`
	SlowRequests        int64             `json:"slow requests"`
	MemoryPeak          int64             `json:"memory peak"`
	Processes           []PoolProcess     `json:"processes"`
	ProcessesCpu        *float64          `json:"processes_cpu"`
	ProcessesMemory     *float64          `json:"processes_memory"`
	Config              map[string]string `json:"config,omitempty"`
	OpcacheStatus       OpcacheStatus     `json:"opcache_status,omitempty"`
	PhpInfo             Info              `json:"php_info,omitempty"`
}

type Result struct {
	Timestamp time.Time
	Pools     map[string]Pool
	Global    map[string]string `json:"global_config,omitempty"`
}

// PoolOutcome is what happened to one configured pool during a scrape: either a
// Result, or the error that prevented one. Failures are values rather than log
// lines so the collector can emit up=0 for a pool that did not answer -- a pool
// that silently vanishes from the output is indistinguishable from a pool that
// was removed from the configuration.
type PoolOutcome struct {
	// Name is the pool's configured or discovered name, used to label a
	// failure. A successful scrape prefers the name PHP-FPM itself reports.
	Name   string
	Socket string
	Result *Result
	Err    error
}

// checkIsStatusFor decides whether a decoded response is this pool's status
// page at all.
//
// A status page always names its pool. Anything else — an empty object, a
// health endpoint, an application's own JSON at the same path — decodes into a
// zero-valued Pool and used to come back with no error: a pool reported as
// running no workers and having served nothing, which a caller that sizes pools
// reads as a site nobody is using, and whose memory it then hands to the
// neighbours.
//
// And it must be the pool that was ASKED for. A status path pointing at another
// pool's socket returns that pool's numbers under this pool's name everywhere
// downstream, so one site is measured and another site's ceiling is written
// from it.
//
// Separated from the wire so it can be tested: making Go's own FastCGI
// responder talk to this client is a protocol interop problem and not this
// rule, and the rule is what has consequences.
func checkIsStatusFor(reported, asked, path string) error {
	if reported == "" {
		return fmt.Errorf("the response at %s carries no pool name, so it is not a "+
			"PHP-FPM status page", path)
	}
	if asked != "" && !strings.EqualFold(reported, asked) {
		return fmt.Errorf("the status page at %s reports pool %q, not %q",
			path, reported, asked)
	}

	return nil
}

// ScrapeAll scrapes every target. It returns one outcome per target, in the
// order given, and an error only when nothing at all could be collected.
//
// It took fpm-exporter's whole *config.Config before this package was extracted.
// A library takes the work it is asked to do; deciding which pools exist belongs
// to the caller.
//
// log may be nil.
func ScrapeAll(ctx context.Context, targets []Target, log *slog.Logger) ([]PoolOutcome, error) {
	log = logOrDiscard(log)

	outcomes := make([]PoolOutcome, len(targets))

	// Pools are scraped concurrently. Serially, each unreachable pool cost its
	// full dial timeout before the next one was tried -- three dead pools took
	// a measured 9.0s of a 15s budget, and healthy pools later in the list
	// returned nothing at all. Bounded, because each pool in flight is a
	// FastCGI connection plus, for opcache, a PHP request.
	// One snapshot of the process table for the whole round, taken before the
	// pools are scraped and shared by all of them. Every pool's subtree is then
	// measured against the same instant — which is what makes their numbers
	// comparable — and the walk of /proc is paid for once rather than once per
	// pool on a host running forty of them.
	snap := snapshotProcesses()

	var wg sync.WaitGroup
	slots := make(chan struct{}, min(len(targets), max(4, runtime.NumCPU())))

	for i, target := range targets {
		wg.Add(1)
		go func(i int, target Target) {
			defer wg.Done()

			slots <- struct{}{}
			defer func() { <-slots }()

			outcome := scrape(ctx, target, snap, log)
			if outcome.Err != nil {
				log.Warn("Pool scrape failed",
					"pool", outcome.Name, "socket", outcome.Socket, "err", outcome.Err)
			}
			// Written by index so the results stay in configuration order.
			outcomes[i] = outcome
		}(i, target)
	}

	wg.Wait()

	if len(outcomes) > 0 && !slices.ContainsFunc(outcomes, func(o PoolOutcome) bool { return o.Err == nil }) {
		return outcomes, fmt.Errorf("all %d PHP-FPM pools failed to scrape", len(outcomes))
	}

	return outcomes, nil
}

// Scrape reads one pool's status page. Its client and response body close at the
// end of this call rather than at the end of a whole batch.
//
// It takes its own snapshot of the process table to measure worker subtrees;
// ScrapeAll shares one across pools instead. See scrape.
//
// log may be nil.
func Scrape(ctx context.Context, target Target, log *slog.Logger) PoolOutcome {
	return scrape(ctx, target, snapshotProcesses(), log)
}

// scrape is Scrape with the process snapshot supplied, so a batch can build one
// snapshot for the whole round. A nil snapshot is valid — subtree RSS is then
// left unmeasured and only each worker's own RSS is filled.
//
// log may be nil.
func scrape(ctx context.Context, target Target, snap *processSnapshot, log *slog.Logger) PoolOutcome {
	log = logOrDiscard(log)

	outcome := PoolOutcome{Name: target.label(), Socket: target.statusAddress()}

	result := &Result{
		Timestamp: time.Now(),
		Pools:     make(map[string]Pool),
		Global:    make(map[string]string),
	}

	scheme, address, path, err := ParseAddress(target.statusAddress(), target.StatusPath)
	if err != nil {
		outcome.Err = fmt.Errorf("invalid FPM socket address: %w", err)
		return outcome
	}

	// The timeout bounds the whole exchange, not just the dial. It used to cover
	// the connect and then hand the parent context — often context.Background()
	// — to the request, so a server that accepted the socket and never sent
	// END_REQUEST hung the scrape indefinitely. A pool being scraped is a pool
	// that is unwell often enough for that to matter.
	ctx, cancel := context.WithTimeout(ctx, target.dialTimeout())
	defer cancel()

	log.Debug("Dialing FastCGI", "scheme", scheme, "address", address, "status_path", path)
	client, err := fcgx.DialContext(ctx, scheme, address)
	if err != nil {
		outcome.Err = fmt.Errorf("failed to dial FastCGI: %w", err)
		return outcome
	}
	defer func() { _ = client.Close() }()

	env := map[string]string{
		"SCRIPT_FILENAME": path,
		"SCRIPT_NAME":     path,
		"SERVER_SOFTWARE": serverSoftware,
		"REMOTE_ADDR":     "127.0.0.1",
		"QUERY_STRING":    "json&full",
	}
	log.Debug("Sending FCGI request", "env", env)

	resp, err := client.Get(ctx, env)
	if err != nil {
		outcome.Err = fmt.Errorf("fcgi GET failed: %w", err)
		return outcome
	}
	defer func() { _ = resp.Body.Close() }()

	var pool Pool
	if err := fcgx.ReadJSON(resp, &pool); err != nil {
		outcome.Err = fmt.Errorf("failed to parse FPM status JSON: %w", err)
		return outcome
	}

	if err := checkIsStatusFor(pool.Name, target.Name, path); err != nil {
		outcome.Err = err

		return outcome
	}

	pool.Address = address
	pool.Path = path
	// The CALLER's context, not a background thirty seconds of this package's
	// own. A scrape given a two-second budget could sit here for thirty inside a
	// call it made — so a wedged binary stopped a loop that had explicitly said
	// how long it was willing to wait.
	if conf, err := ParseConfigContext(ctx, target.Binary, target.ConfigPath); err == nil {
		// Exact first. PHP-FPM section names are case-SENSITIVE, so a host with
		// pools called `api` and `API` has two different pools — and matching
		// case-insensitively over a map, whose iteration order is random, gave
		// whichever the range happened to reach last. For a caller that writes
		// pm.max_children from this, that is acting on another site's numbers,
		// differently on each run.
		if values, ok := conf.Pools[pool.Name]; ok {
			pool.Config = exportableConfig(values)
		} else {
			// Case-insensitively only when it is unambiguous, for the operator
			// who wrote [Www] in one file and www in another.
			var match map[string]string
			found := 0
			for section, values := range conf.Pools {
				if strings.EqualFold(section, pool.Name) {
					match = values
					found++
				}
			}
			if found == 1 {
				pool.Config = exportableConfig(match)
			}
		}
		result.Global = exportableConfig(conf.Global)
	}

	// PHP-FPM does not report worker memory; it has to be read from the OS using
	// the pids the status page provides. See rss.go.
	enrichWorkerRSS(&pool, snap, log)

	recountProcesses(&pool, target.StatusPath)

	phpStatus, err := GetPHPStats(ctx, target)
	if err == nil && phpStatus != nil {
		pool.PhpInfo = *phpStatus
	} else {
		log.Debug("failed to get PHP info", "error", err)
	}

	opcacheStatus, err := GetOpcacheStatus(ctx, target)
	if err == nil && opcacheStatus != nil {
		pool.OpcacheStatus = *opcacheStatus
	} else {
		log.Debug("failed to get Opcache info", "error", err)
	}

	result.Pools[pool.Name] = pool
	// Only adopt the name PHP-FPM reports when the operator did not choose one;
	// a configured name has to survive the pool going down and coming back.
	if target.Name == "" && pool.Name != "" {
		outcome.Name = pool.Name
	}
	outcome.Result = result

	return outcome

}

// recountProcesses derives the pool's process counts and per-request averages
// from the process list, rather than trusting the summary fields, and strips
// the query string from each request URI.
//
// Extracted so it can be tested: the previous test copied this logic into
// itself and asserted on the copy, so no production statement ran.
func recountProcesses(pool *Pool, statusPath string) {
	var totalCPU, totalMem float64
	var count int

	// PHP-FPM reports the full request URI of each worker, query string
	// included -- on a real site that is a rolling sample of production URLs
	// with their tokens in them, and /json republishes it unauthenticated. The
	// path is enough for the filter below and for debugging.
	for i := range pool.Processes {
		if q := strings.IndexByte(pool.Processes[i].RequestURI, '?'); q >= 0 {
			pool.Processes[i].RequestURI = pool.Processes[i].RequestURI[:q]
		}
	}
	var activeCount, idleCount int64

	for _, proc := range pool.Processes {
		// Count processes by state
		switch strings.ToLower(proc.State) {
		case "running", "reading headers", "info", "finishing", "ending":
			activeCount++
		case "idle":
			idleCount++
		}

		// CPU/memory calculation, excluding the exporter's own traffic: the
		// status call and the opcache probe. Counting those skews the averages
		// worst on idle pools, where they are most of the traffic.
		if !strings.HasPrefix(proc.RequestURI, statusPath) &&
			!strings.HasPrefix(proc.RequestURI, "/"+opcacheScriptPrefix) {

			totalCPU += float64(proc.LastRequestCPU)
			totalMem += float64(proc.LastRequestMemory)
			count++
		}
	}

	// Recalculate process counts from actual process list
	pool.ActiveProcesses = activeCount
	pool.IdleProcesses = idleCount
	pool.TotalProcesses = int64(len(pool.Processes))

	if count > 0 {
		pool.ProcessesCpu = ptr(totalCPU / float64(count))
		pool.ProcessesMemory = ptr(totalMem / float64(count))
	}

}

// exportedConfigKeys are the only pool-config settings that leave this process.
// `php-fpm -tt` dumps the effective configuration, which routinely carries
// secrets -- env[DB_PASSWORD], env[APP_KEY], php_admin_value[...] -- and the
// whole map used to be serialised on the unauthenticated /json endpoint. These
// eleven are exactly the keys the collector turns into metrics.
var exportedConfigKeys = map[string]bool{
	"pm.max_children":           true,
	"pm.max_requests":           true,
	"pm.max_spare_servers":      true,
	"pm.max_spawn_rate":         true,
	"pm.min_spare_servers":      true,
	"pm.process_idle_timeout":   true,
	"pm.start_servers":          true,
	"request_slowlog_timeout":   true,
	"request_terminate_timeout": true,
	"rlimit_core":               true,
	"rlimit_files":              true,
}

// exportableConfig copies through only the settings we publish. An allow-list,
// not a deny-list: a new PHP-FPM directive holding a credential must not become
// a leak because nobody remembered to add it to a block list.
func exportableConfig(values map[string]string) map[string]string {
	exportable := make(map[string]string, len(exportedConfigKeys))
	for key, value := range values {
		if exportedConfigKeys[strings.ToLower(strings.TrimSpace(key))] {
			exportable[key] = value
		}
	}
	return exportable
}

func ptr[T any](v T) *T {
	return &v
}

func ParseAddress(addr string, path string) (scheme, address, scriptPath string, err error) {
	if strings.HasPrefix(addr, "unix://") {
		return "unix", strings.TrimPrefix(addr, "unix://"), path, nil
	}
	if strings.HasPrefix(addr, "tcp://") {
		return "tcp", strings.TrimPrefix(addr, "tcp://"), path, nil
	}
	if strings.HasPrefix(addr, "/") {
		return "unix", addr, path, nil
	}
	return "", "", "", fmt.Errorf("unsupported socket format: %s", addr)
}
