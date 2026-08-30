---
title: Quickstart
weight: 1
description: Install it and read a host in one short program.
---

# Quickstart

```bash
go get github.com/cboxdk/phpfpm
```

Find every master on the host and scrape the pools it serves:

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

	outcomes, _ := phpfpm.ScrapeAll(ctx, targets, log)
	for _, o := range outcomes {
		if o.Err != nil {
			fmt.Printf("%s: unreachable: %v\n", o.Name, o.Err)
			continue
		}
		for name, pool := range o.Result.Pools {
			fmt.Printf("%s: %d active, %d accepted\n",
				name, pool.ActiveProcesses, pool.AcceptedConnections)
		}
	}
}
```

`ScrapeAll` returns one outcome per target, in order, and an error only when
nothing at all could be collected. A pool that could not be reached comes back
with `Err` set rather than vanishing — it still occupies memory, and dropping it
would hand its share to its neighbours.

Next: [reading a host](guides/reading-a-host.md) in more detail, or — if you are
going to *change* a configuration — [reloading a master safely](guides/reloading-safely.md),
which is where the care is.
