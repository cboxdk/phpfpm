---
title: The configuration cache
weight: 3
description: ParseConfig caches for the life of the process; anything that changes a configuration must invalidate it.
---

# The configuration cache

`ParseConfig` runs `php-fpm -tt` and parses the effective configuration. Forking
php-fpm is not free, so the result is cached per binary+config pair for the life
of the process. On a scrape loop that reads the same configuration every few
seconds, that is exactly right.

It is wrong for anything that *changes* the configuration.

## The trap

The cache never expires on its own. A caller that reloads a master onto a new
configuration and then calls `ParseConfig` again gets the settings it saw at
startup, not the ones it just wrote — until the process restarts. A consumer that
missed this reported a pool as configured for 4 workers hours after setting it to
12.

## The fix

Invalidate the cache after you change a configuration:

```go
// ... write the drop-in, reload the master ...
phpfpm.InvalidateConfigCache(master.Binary, master.ConfigPath)
```

The invalidation is keyed on the same binary and config path as the parse, and
the paths are cleaned first, so `/etc/php/../php-fpm.conf` and
`/etc/php-fpm.conf` — the same file — are the same cache entry. An invalidation
that arrives while a parse is in flight is honoured: the stale result is dropped
rather than stored, so a scrape that began before your change cannot repopulate
the cache with pre-change data.

## Passing a deadline

`ParseConfig` uses a default timeout so a wedged binary cannot hang a scrape loop
forever. If you have your own deadline — a scrape budget, a request context — pass
it with `ParseConfigContext`, which forks in its own process group and kills the
group on cancellation, so nothing the binary started outlives the timeout.
