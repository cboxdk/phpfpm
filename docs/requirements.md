---
title: Requirements
weight: 2
description: What the library needs, and where it runs.
---

# Requirements

- **Go 1.26 or newer.**
- **A host running PHP-FPM.** The library shells out to the `php-fpm` binary for
  `-tt` (parse the effective configuration) and `-t` (validate one), scans
  `/proc` to find masters, and signals them. It runs where php-fpm runs, which in
  practice means Linux. On other platforms the process scan and the trust checks
  degrade rather than pretend.
- **The right user for what you are doing.** Discovery reads other processes'
  details, and before it executes or signals anything it checks that the binary
  and config are owned by root or by the calling process. Running as root reads
  everything and applies the strict directory-ownership checks; running as the
  php-fpm user reads its own master; running as a stranger sees less, on purpose.
  See [Identity and trust](design/identity-and-trust.md).

The library holds no long-running state and starts no goroutines of its own
beyond the lifetime of a single call. Its only caches are the parsed
configuration (per binary+config pair) and the PHP version/extension lookup;
both are process-lifetime and the first must be invalidated when you change a
configuration — see [The configuration cache](guides/the-config-cache.md).
