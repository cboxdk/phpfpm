---
title: Guides
weight: 10
description: Reading a host, reloading a master safely, and the configuration cache you must invalidate if you change anything.
---

# Guides

- **[Reading a host](reading-a-host.md)** — discovery and scraping, and why a
  pool that could not be reached is reported rather than dropped.
- **[Reloading a master safely](reloading-safely.md)** — the validate-then-reload
  sequence that is the whole safety story, and confirming the master survived.
- **[The configuration cache](the-config-cache.md)** — `ParseConfig` caches for
  the life of the process; anything that *changes* a configuration must
  invalidate it.
