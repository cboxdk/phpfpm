---
title: Reloading a master safely
weight: 2
description: The validate-then-reload sequence that is the whole safety story, and confirming the master survived.
---

# Reloading a master safely

Changing a pool's settings means writing configuration and telling the master to
re-read it. The order is not a preference — it is the difference between a
graceful reload and a dead host.

## Validate, then reload

A configuration php-fpm rejects does not degrade politely. The master refuses to
come back, and every pool it served goes with it. So a caller changing `pm.*`
must prove php-fpm accepts the configuration *before* anything is signalled:

```go
// 1. Ask php-fpm whether it accepts the configuration, without applying it.
if err := phpfpm.Validate(ctx, master.Binary, master.ConfigPath); err != nil {
	return fmt.Errorf("php-fpm rejected the configuration: %w", err)
}

// 2. Only now reload.
newPID, err := phpfpm.ReloadAndWait(ctx, phpfpm.ReloadTarget{
	PID:        master.PID,
	ConfigPath: master.ConfigPath,
}, 2*time.Second, log)
if err != nil {
	return fmt.Errorf("the master did not survive the reload: %w", err)
}
```

`Validate` runs `php-fpm -t` against the configuration and returns the rejection
verbatim if there is one. Verified against a real master: a drop-in naming a pool
that no longer exists makes `php-fpm -t` exit 78, and reloading with that file
present kills the master permanently.

## Confirm the master, not the pid

`ReloadAndWait` sends SIGUSR2 and then watches the master actually survive the
settle window you give it. Two things make this more than "is the pid still
alive":

- **A daemonized master re-execs into a new pid.** In the foreground the pid
  survives; daemonized — php-fpm's own default — the master replaces itself and
  the original exits. `ReloadAndWait` follows the master across the re-exec and
  returns the pid it came back as. A caller watching the pid it signalled would
  report a textbook reload as a dead master.
- **A master that fails late initialisation dies a moment after the signal, not
  instantly.** So the settle window is watched to its end rather than accepted at
  the first glimpse of a live process — a master that exists for a moment and then
  exits is caught, not mistaken for success.

If the master genuinely dies with nothing taking its place, `ReloadAndWait`
returns an error. The change did not take, and the caller's job is to put the
previous configuration back.

## Validation is not the same as survival

`Validate` forks a separate process with no sockets to bind and no pools to
start. A live master can still fail to initialise on a configuration that
validated — a socket it cannot bind, a permission it does not have. That is
exactly why the reload is watched rather than trusted: validation is necessary,
and it is not sufficient.

For the reload semantics in full, see [Reload, not restart](../design/reload-not-restart.md).
