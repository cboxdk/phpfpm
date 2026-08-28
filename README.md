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
| Find running masters | `Discover` — locates php-fpm processes, their pools, and the master PID |
| Scrape live status | `Scrape` (one pool) and `ScrapeAll` (many, concurrently) — per-pool counters and per-worker RSS |
| Read opcache state | `GetOpcacheStatus` |
| Validate and reload | `Validate` (`php-fpm -t`), `Reload` (SIGUSR2), `ReloadAndWait`, `MasterPID` |

## Design notes

**No package-level logger.** Every entry point that can log takes a
`*slog.Logger`. A library that owns a global logger cannot be embedded twice with
different destinations, and it makes tests order-dependent.

**Reload, never restart.** `Reload` sends SIGUSR2, which makes the master re-read
its configuration and cycle workers gracefully. Callers that change `pm.*` must
pair it with `Validate` first: an invalid drop-in that reaches a reload does not
degrade — the master refuses to come back, and every pool it served goes with it.
Verified against a real master: a drop-in naming a pool that no longer exists
makes `php-fpm -t` exit 78, and reloading with that file present kills the master
permanently.

**The configuration cache never expires on its own.** `ParseConfig` forks
`php-fpm`, so its result is cached per binary+config pair for the life of the
process. That is right for a scrape loop and wrong for anything that CHANGES the
configuration: such a caller must call `InvalidateConfigCache` after reloading,
or it will keep reading the settings it saw at startup. A consumer that missed
this reported a pool as configured for 4 workers hours after setting it to 12.

## Status

Extracted from fpm-exporter's `internal/phpfpm`, where it could not be imported.
The behaviour and its test fixtures moved unchanged.

## License

MIT
