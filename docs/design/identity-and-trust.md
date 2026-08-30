---
title: Identity and trust
weight: 2
description: Why the process table is not a trust boundary, and what is checked before a binary is executed or a pid is signalled.
---

# Identity and trust

This library forks the `php-fpm` binary and signals master processes. Both are
things you must not do to a target an attacker controls, and the process table
does not tell you who controls one.

## The process table is not a trust boundary

Any local user can start a process whose name matches php-fpm's and whose command
line names a config path they own. Discovery reads both the executable and the
config path off that process and hands them to `exec`. So before either is run,
they are checked:

- **Absolute paths only.** A relative name would be resolved through `PATH`, which
  is the attacker's variable, not the library's.
- **No symlinks.** A symlink can be repointed after the check, and its own
  ownership says nothing about the target's.
- **Owned by root or by this process, and not writable by others.**
- **Running as root, the directories above them too** — because a root-owned
  binary inside a directory anyone can write to can be swapped between the check
  and the exec.

A path that fails these is refused, not run. A path that is simply *missing* — a
leftover master whose config was removed, of which any busy host has several — is
distinguished from one that fails the trust checks, so a caller can log it quietly
rather than treating a tidy-up as an attack.

## Checked at the point of use, not just at discovery

The checks run wherever a binary is about to be executed, not only when it is
discovered. A consumer can supply a binary path from somewhere other than the
process scan — a state file, a configuration — and a path from a file an attacker
can write is a binary this process would otherwise run, often as root. Validating
at the point of `exec` rather than at the point of discovery is the difference
between guarding one path and guarding the property.

## A pid is a promise about one instant

A pid identifies a process only at the moment it is read. A master can exit
between discovery and a reload, and the kernel can hand the number to something
else. So `VerifyMaster` requires the process name php-fpm uses, an executable that
passes the ownership checks, and — for `ReloadMaster` — the config path compared
as a path rather than a substring, so on a host running two masters the signal
reaches the intended one and not its neighbour.

`ReloadAndWait` goes further and compares the process's start time across the
settle window: a pid that is reused mid-reload is a different process wearing the
same number, and the start time is what tells them apart. Signalling the wrong
pid is the worst thing this library can do, so the identity is confirmed before
the signal and again before success is reported.
