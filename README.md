# phpfpm

Go library for talking to PHP-FPM: pool discovery, effective-configuration
parsing, status scraping over FastCGI, and control operations.

It is the shared domain layer beneath
[fpm-exporter](https://github.com/cboxdk/fpm-exporter) (which reads) and
[fpm-tune](https://github.com/cboxdk/fpm-tune) (which reads and writes). Neither
depends on the other; both depend on this.

## What it does

| Area | Entry point |
|---|---|
| Parse the effective configuration | `ParseConfig` — runs `php-fpm -tt` and returns global settings plus one map per pool |
| Find pools | `Discover` — locates php-fpm masters and the pools they serve, with each pool's configured size |
| Find masters only | `DiscoverMasters` — the same scan without parsing any configuration |
| Scrape live status | `Scrape` (one pool) and `ScrapeAll` (many, concurrently) — per-pool counters and per-worker RSS |
| Read opcache state | `GetOpcacheStatus` |
| Validate | `Validate` — `php-fpm -t` against a config, without applying it |
| Reload | `ReloadMaster` (SIGUSR2, scoped to a named config), `ReloadAndWait`, `VerifyMaster`, `MasterPID` |

## Design notes

**No package-level logger.** Every entry point that can log takes a
`*slog.Logger`. A library that owns a global logger cannot be embedded twice with
different destinations, and it makes tests order-dependent.

**Reload, never restart.** SIGUSR2 makes the master re-read its configuration and
cycle workers gracefully, carrying its listening sockets across so no request is
dropped. Callers that change `pm.*` must pair it with `Validate` first: an invalid
drop-in that reaches a reload does not degrade — the master refuses to come back,
and every pool it served goes with it. Verified against a real master: a drop-in
naming a pool that no longer exists makes `php-fpm -t` exit 78, and reloading with
that file present kills the master permanently.

**A reload does not always preserve the pid.** SIGUSR2 makes the master re-exec
itself. In the foreground — under systemd, or as pid 1 in a container — the pid
survives. Running DAEMONIZED, which is php-fpm's own default, the re-exec produces
a new process and the original exits. `ReloadAndWait` therefore confirms the
MASTER rather than the number, returns the pid it came back as, and reports a
master that genuinely died with nothing taking its place. A consumer that watched
the pid it signalled reported a textbook reload as a dead master and rolled the
change back.

**Identity before signalling.** SIGUSR2 terminates a process that has no handler
for it, and a pid is only a promise about the instant it was read: a master can
exit between discovery and the reload, and the kernel can hand the number to
something else. `VerifyMaster` requires the process name php-fpm uses, an
executable that passes the ownership checks, and — for `ReloadMaster` — the config
path compared as a path rather than a substring, so on a host running two masters
the signal reaches the intended one.

**The process table is not a trust boundary.** Any local user can start a process
whose name matches and whose command line names a config path they control, and
both are handed to `exec`. Discovery therefore checks that the binary and config
are owned by root or by this process and are not writable by anyone else, refuses
relative paths (which `exec` would resolve through `PATH`), and — when running as
root — checks the directories above them too, since a root-owned binary inside a
directory anyone can write to can be swapped between the check and the exec.

**A master can be found without reading its configuration.** `Discover` parses
each master's effective config to enumerate pools, which means a master whose
configuration no longer parses is invisible to it — exactly when a consumer trying
to REPAIR that configuration most needs to find it. `DiscoverMasters` reads only
the process table and answers regardless.

**The configuration cache never expires on its own.** `ParseConfig` forks
`php-fpm`, so its result is cached per binary+config pair for the life of the
process. That is right for a scrape loop and wrong for anything that CHANGES the
configuration: such a caller must call `InvalidateConfigCache` after reloading, or
it will keep reading the settings it saw at startup. A consumer that missed this
reported a pool as configured for 4 workers hours after setting it to 12. An
invalidation that arrives while a parse is in flight is honoured: the stale result
is dropped rather than stored.

**A pool that cannot be scraped still occupies memory.** `Discovered` and `Target`
carry the pool's configured `pm.max_children` and process manager, so a caller
holding a failed scrape still knows how large the pool is. Without it, a site
restarting for five seconds looks like a pool needing nothing, and its memory is
handed to its neighbours.

## Status

Extracted from fpm-exporter's `internal/phpfpm`, where it could not be imported.
The parsing and its fixtures moved unchanged; the control and trust surfaces have
been substantially reworked since, under review and against real masters.

## License

MIT
