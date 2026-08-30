---
title: phpfpm
weight: 0
description: A Go library for talking to PHP-FPM — discover masters, parse configuration, scrape status, and reload safely.
---

# phpfpm

A Go library for talking to PHP-FPM. It discovers running masters, parses their
effective configuration, scrapes live status over FastCGI, validates a
configuration, and reloads a master without dropping a request.

It is the shared layer beneath
[fpm-exporter](https://github.com/cboxdk/fpm-exporter), which reads a host, and
[fpm-tune](https://github.com/cboxdk/fpm-tune), which reads and writes one.
Neither depends on the other; both depend on this.

```bash
go get github.com/cboxdk/phpfpm
```

## The mental model

The library is a set of standalone functions over a few small value types. There
is no client to construct and no connection to hold open — you call `Discover`,
`Scrape`, `Validate` or `ReloadAndWait` when you need an answer, and pass a
`*slog.Logger` to the ones that can log.

Two ideas run through all of it:

- **A pid is a promise about the instant it was read.** A master can exit between
  discovery and a reload, and the kernel can reuse the number — so anything that
  signals a process verifies its identity first, and confirms the master rather
  than the number afterwards. See [Identity and trust](design/identity-and-trust.md).
- **The process table is not a trust boundary.** Any local user can start a
  process that looks like php-fpm and names a config they control, and both are
  handed to `exec` — so discovery checks ownership and refuses paths it should not
  run. See [Identity and trust](design/identity-and-trust.md).

## The surface

| Task | Entry point |
|---|---|
| Find masters and their pools | `Discover` |
| Find masters only | `DiscoverMasters` |
| Parse effective configuration | `ParseConfig`, `ParseConfigContext` |
| Scrape live status | `Scrape`, `ScrapeAll` |
| Read opcache | `GetOpcacheStatus` |
| Validate a configuration | `Validate` |
| Reload a master | `ReloadAndWait`, `ReloadMaster` |
| Confirm a master's identity | `VerifyMaster`, `MasterPID` |
| Invalidate the parse cache | `InvalidateConfigCache` |

Full reference: [pkg.go.dev/github.com/cboxdk/phpfpm](https://pkg.go.dev/github.com/cboxdk/phpfpm).

## The sections

- **[Guides](guides/_index.md)** — reading a host, reloading a master safely, and
  the configuration cache you have to invalidate if you change anything.
- **[Design](design/_index.md)** — the reload semantics and the identity/trust
  model, which are the parts that refuse to do the obvious thing because the
  obvious thing takes a host down.
