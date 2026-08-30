---
title: Reload, not restart
weight: 1
description: Why SIGUSR2 and not a restart, why a reload does not preserve the pid, and why the master rather than the number gets confirmed.
---

# Reload, not restart

## Why a reload at all

SIGUSR2 makes a php-fpm master re-read its configuration and cycle its workers
gracefully, carrying its listening sockets across so no in-flight request is
dropped. A restart tears the sockets down. For a tool that adjusts `pm.*`
settings on a live host, a reload is the only acceptable way to apply them — but
it comes with two sharp edges.

## A reload does not preserve the pid

SIGUSR2 makes the master re-exec itself. In the foreground — under systemd, or as
pid 1 in a container — the pid survives the re-exec. Running *daemonized*, which
is php-fpm's own default, the re-exec produces a new process and the original
exits.

So a caller that signalled pid *N* and then checked whether pid *N* was still
alive would, on the common daemonized setup, see it gone and conclude the master
died — and roll back a change that had actually succeeded. `ReloadAndWait`
handles this: when the signalled pid disappears, it looks for the successor
master serving the same configuration, follows it, and returns the pid it came
back as. It reports a dead master only when the pid is gone *and* nothing has
taken its place.

## A late failure is not an instant one

A master that fails to initialise on the new configuration does not vanish the
instant it is signalled — it exists for a moment, tries, and then exits. So the
settle window is watched to its end rather than accepted at the first sight of a
live process. Returning success at the first glimpse of a successor would report
a master that was about to go down as healthy, which is the exact failure the
settle window exists to catch.

## A zombie is not alive

The liveness check asks the kernel with signal 0, which succeeds against a
process that has exited and is merely waiting to be collected. On Linux the check
also reads the process state, because a zombie master is a dead master — it has
exited and cannot serve anything — and on an init that does not reap, the plain
signal-0 check would call it alive forever.

## The consequence for callers

If you signal a master yourself with `ReloadMaster`, you get the reload and
nothing more. If you want the reload *confirmed*, use `ReloadAndWait` and check
its error: a nil error means a master is serving the configuration after the
settle window, whatever pid it wears; a non-nil error means the master did not
survive, and the previous configuration should go back. See
[reloading safely](../guides/reloading-safely.md).
