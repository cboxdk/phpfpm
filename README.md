# phpfpm

A Go library for talking to PHP-FPM: discover running masters, parse their
effective configuration, scrape live status over FastCGI, and reload a master
safely.

It is the shared layer beneath
[fpm-exporter](https://github.com/cboxdk/fpm-exporter), which reads a host, and
[fpm-tune](https://github.com/cboxdk/fpm-tune), which reads and writes one.
Neither depends on the other; both depend on this.

```bash
go get github.com/cboxdk/phpfpm
```

## Requirements

- **Go 1.26 or newer.**
- **A host running PHP-FPM.** The library shells out to the `php-fpm` binary for
  `-tt` (parse) and `-t` (validate), scans `/proc` to find masters, and signals
  them — so it runs where php-fpm runs, which in practice means Linux. On other
  platforms the process scan and the trust checks degrade rather than pretend.
- **The right user.** Discovery reads other processes' details and, before it
  executes or signals anything, checks that the binary and config are owned by
  root or by this process. Running as root reads everything and applies the
  strict checks; running as the php-fpm user reads its own master; running as a
  stranger sees less, on purpose.

## Reading a host

Find every master and scrape the pools it serves:

```go
package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/cboxdk/phpfpm"
)

func main() {
	log := slog.Default()

	discovered, err := phpfpm.Discover(log)
	if err != nil {
		panic(err)
	}

	targets := make([]phpfpm.Target, 0, len(discovered))
	for _, d := range discovered {
		targets = append(targets, phpfpm.TargetFromDiscovered(d))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// One outcome per target, in order. A pool that could not be reached comes
	// back with Err set rather than vanishing — it still occupies memory, and
	// dropping it would hand its share to its neighbours.
	outcomes, _ := phpfpm.ScrapeAll(ctx, targets, log)
	for _, o := range outcomes {
		if o.Err != nil {
			fmt.Printf("%s: unreachable: %v\n", o.Name, o.Err)
			continue
		}
		for name, pool := range o.Result.Pools {
			fmt.Printf("%s: %d active, %d accepted, %d workers\n",
				name, pool.ActiveProcesses, pool.AcceptedConnections, len(pool.Processes))
		}
	}
}
```

## Reloading a master safely

Changing a pool's settings means writing configuration and telling the master to
re-read it. The order is the whole safety story: **validate, then reload**, or an
invalid file takes the master down and every pool with it.

```go
// 1. Prove php-fpm accepts the configuration before anything is signalled.
if err := phpfpm.Validate(ctx, master.Binary, master.ConfigPath); err != nil {
	return fmt.Errorf("php-fpm rejected the configuration: %w", err)
}

// 2. Reload, and watch the master actually survive it. A daemonized master
//    re-execs into a NEW pid on SIGUSR2 — this follows it, and reports a master
//    that genuinely died rather than one that merely changed number.
newPID, err := phpfpm.ReloadAndWait(ctx, phpfpm.ReloadTarget{
	PID:        master.PID,
	ConfigPath: master.ConfigPath,
}, 2*time.Second, log)
if err != nil {
	return fmt.Errorf("the master did not survive the reload: %w", err)
}
```

## The surface

| Task | Entry point |
|---|---|
| Find masters and their pools | `Discover` — process scan plus `php-fpm -tt`, with each pool's configured size |
| Find masters only | `DiscoverMasters` — the same scan without parsing any configuration |
| Parse effective configuration | `ParseConfig` / `ParseConfigContext` — global settings plus one map per pool |
| Scrape live status | `Scrape` (one pool), `ScrapeAll` (many, concurrently) — per-pool counters, and per worker both its own RSS and its whole subtree's (the ffmpeg it spawned) |
| Read opcache | `GetOpcacheStatus` |
| Validate a configuration | `Validate` — `php-fpm -t`, without applying it |
| Reload a master | `ReloadAndWait`, `ReloadMaster` (SIGUSR2, scoped to a named config) |
| Confirm a master's identity | `VerifyMaster`, `MasterPID` |
| Invalidate the parse cache | `InvalidateConfigCache` — after you change a configuration |

Full reference: [pkg.go.dev/github.com/cboxdk/phpfpm](https://pkg.go.dev/github.com/cboxdk/phpfpm).
Longer-form documentation is in [`docs/`](docs/index.md): guides for
[reading a host](docs/guides/reading-a-host.md),
[reloading safely](docs/guides/reloading-safely.md), and
[the configuration cache](docs/guides/the-config-cache.md), plus the design
notes below in full.

## Why it is shaped the way it is

The interesting parts of this library are the ones that refuse to do the obvious
thing, because the obvious thing is wrong in a way that only shows up in
production. Each of these was found against a real master.

**Reload, never restart.** SIGUSR2 makes the master re-read its configuration and
cycle workers gracefully, carrying its listening sockets across so no request is
dropped. But a configuration php-fpm rejects does not degrade — the master
refuses to come back, and every pool it served goes with it. So a caller changing
`pm.*` must pair the reload with `Validate` first. Verified: a drop-in naming a
pool that no longer exists makes `php-fpm -t` exit 78, and reloading with that
file present kills the master permanently.

**A reload does not preserve the pid.** SIGUSR2 makes the master re-exec itself.
In the foreground — under systemd, or as pid 1 in a container — the pid survives.
Daemonized, which is php-fpm's own default, the re-exec produces a new process and
the original exits. `ReloadAndWait` confirms the *master* rather than the number,
returns the pid it came back as, and reports one that genuinely died with nothing
taking its place. A consumer that watched the pid it signalled reported a textbook
reload as a dead master and rolled a good change back.

**Identity before signalling.** A pid is a promise about the instant it was read.
A master can exit between discovery and the reload, and the kernel can reuse the
number for something else — so `ReloadAndWait` also compares the process's start
time across the settle window, and `ReloadMaster` compares the config path as a
path rather than a substring, so on a host running two masters the signal reaches
the intended one.

**The process table is not a trust boundary.** Any local user can start a process
whose name matches php-fpm's and whose command line names a config they control,
and both are handed to `exec`. Discovery checks that the binary and config are
owned by root or by this process and not writable by others, refuses relative
paths (which `exec` resolves through `PATH`), and — as root — checks the
directories above them, since a root-owned binary inside a world-writable
directory can be swapped between the check and the exec.

**A master can be found without reading its configuration.** `Discover` parses
each master's config to enumerate pools, so a master whose configuration no longer
parses is invisible to it — exactly when a consumer trying to *repair* that
configuration most needs to find it. `DiscoverMasters` reads only the process
table and answers regardless.

**The configuration cache never expires on its own.** `ParseConfig` forks
`php-fpm`, so its result is cached per binary+config pair for the life of the
process. That is right for a scrape loop and wrong for anything that *changes* the
configuration: such a caller must call `InvalidateConfigCache` after reloading, or
it keeps reading the settings it saw at startup. A consumer that missed this
reported a pool as configured for 4 workers hours after setting it to 12.

**A pool that cannot be scraped still occupies memory.** `Discovered` and `Target`
carry the pool's configured `pm.max_children` and process manager, so a caller
holding a failed scrape still knows how large the pool is. Without it, a site
restarting for five seconds looks like a pool needing nothing, and its memory is
handed to its neighbours.

**A pool can carry a hint the library does not interpret.** `Discovered` and
`Target` also carry `Workload` — the value of `env[FPM_TUNE_WORKLOAD]` from the
pool's own config, or empty. This package only surfaces it; what "web" or
"subprocess-heavy" means is the consumer's business. The config is the one place
a per-pool hint can live and be discovered without a second file.

**No package-level logger.** Every entry point that can log takes a `*slog.Logger`.
A library that owns a global logger cannot be embedded twice with different
destinations, and it makes tests order-dependent.

## Development

```bash
make check        # fmt, tidy, vet, lint, race, vulncheck, license-check
go test ./...     # the suite runs against a real php-fpm when one is installed
```

## Security

This library execs php-fpm and signals masters, so its trust boundaries matter.
Found a way past one? See [SECURITY.md](SECURITY.md) — please report it privately.

## Provenance

Extracted from fpm-exporter's `internal/phpfpm`, where it could not be imported.
The parsing and its fixtures moved unchanged; the control and trust surfaces have
been reworked substantially since, under review and against real masters.

## License

MIT — see [LICENSE](LICENSE).
