---
title: Design
weight: 20
description: The reload semantics and the identity/trust model — the parts that refuse the obvious thing because the obvious thing takes a host down.
---

# Design

The interesting parts of this library are the ones that refuse to do the obvious
thing, because the obvious thing is wrong in a way that only shows up in
production. Each was found against a real master.

- **[Reload, not restart](reload-not-restart.md)** — why SIGUSR2 and not a
  restart, why a reload does not preserve the pid, and why the master rather than
  the number is what gets confirmed.
- **[Identity and trust](identity-and-trust.md)** — why the process table is not
  a trust boundary, and what is checked before a binary is executed or a pid is
  signalled.
