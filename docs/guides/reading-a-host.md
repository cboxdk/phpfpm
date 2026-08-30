---
title: Reading a host
weight: 1
description: Discovery and scraping, and why a pool that could not be reached is reported rather than dropped.
---

# Reading a host

Reading a host is two steps: find what is running, then ask each pool how it is
doing.

## Discovery

`Discover` scans the process table for php-fpm masters and parses each one's
effective configuration with `php-fpm -tt` to enumerate its pools. It returns a
`[]Discovered`, one per pool, each carrying the pool's name, its sockets, and —
importantly — its configured `pm.max_children` and process manager.

```go
discovered, err := phpfpm.Discover(log)
```

If you only need the masters and not their pools — because you are about to
repair a configuration that no longer parses, for instance — use
`DiscoverMasters`, which reads only the process table and does not fork
`php-fpm`. A master whose configuration no longer parses is invisible to
`Discover` but found by `DiscoverMasters`, which is exactly the case a repair
tool needs.

## Scraping

Turn the discovered pools into `Target`s and scrape them:

```go
targets := make([]phpfpm.Target, 0, len(discovered))
for _, d := range discovered {
	targets = append(targets, phpfpm.TargetFromDiscovered(d))
}
outcomes, err := phpfpm.ScrapeAll(ctx, targets, log)
```

`ScrapeAll` scrapes every target concurrently and returns one `PoolOutcome` per
target, in the order given. Its error is non-nil only when *nothing* could be
collected; a run where some pools answered and others did not succeeds, with the
failures carried as values.

Each outcome is either a `Result` or the `Err` that prevented one:

```go
for _, o := range outcomes {
	if o.Err != nil {
		// This pool did not answer. See below — it still matters.
		continue
	}
	for name, pool := range o.Result.Pools {
		// pool.ActiveProcesses, pool.AcceptedConnections, pool.Processes, ...
	}
}
```

## A failed scrape is a value, not a gap

A pool that could not be reached comes back with `Err` set rather than being
dropped from the results. That is deliberate, and it matters to any caller that
sizes pools by memory: a pool whose socket refuses — restarting, or briefly
overloaded — is not a pool that has stopped occupying memory. A caller that
treated the absence as "this pool needs nothing" would hand its allocation to its
neighbours and overcommit the host the moment it came back.

That is also why `Discovered` and `Target` carry the configured
`pm.max_children`: a caller holding a failed scrape still knows how large the
pool is, from what discovery read, even though the live counters are missing.

## Worker memory

PHP-FPM does not report worker memory in its status page, so the library reads it
from the operating system using the pids the status page provides. A pool with no
status page enabled therefore has counters but no per-worker RSS. Only the pool
that the status response actually names is used — a status endpoint claiming to
be a different pool is refused, not relabelled.
