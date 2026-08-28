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
| Find running masters | `Discover` — locates php-fpm processes and the pools they serve |
| Scrape live status | `Status` — per-pool counters and per-worker RSS over FastCGI |
| Read opcache state | `Opcache` |
| Validate and reload | `Validate` (`php-fpm -t`), `Reload` (SIGUSR2) |

## Design notes

**No package-level logger.** Every entry point that can log takes a
`*slog.Logger`. A library that owns a global logger cannot be embedded twice with
different destinations, and it makes tests order-dependent.

**Reload, never restart.** `Reload` sends SIGUSR2, which makes the master re-read
its configuration and cycle workers gracefully. Callers that change `pm.*` should
always pair it with `Validate` first: an invalid drop-in that reaches a reload
takes the pool down.

**Caching is opt-in.** `ParseConfig` is expensive (it forks `php-fpm`). The
caching wrapper is a separate type so a long-running caller gets reuse while a
one-shot caller gets fresh output.

## Status

Extracted from fpm-exporter's `internal/phpfpm`, where it could not be imported.
The behaviour and its test fixtures moved unchanged.

## License

MIT
