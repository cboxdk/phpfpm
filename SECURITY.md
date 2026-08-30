# Security policy

## Reporting a vulnerability

Please report suspected vulnerabilities **privately**, through GitHub's Private
Vulnerability Reporting:

> [Report a vulnerability](https://github.com/cboxdk/phpfpm/security/advisories/new)
> — the "Report a vulnerability" button under the repository's **Security** tab.

That opens a private advisory only the maintainers can see. Please do not open a
public issue for a security problem before it has been addressed.

This is a young library maintained on a best-effort basis. There is no PGP key,
no security mailbox, and no guaranteed response time — the honest position is
that reports are read and acted on as soon as they can be, not against an SLA we
would be inventing here.

## What is in scope

phpfpm identifies running php-fpm masters and talks to them: it parses their
effective configuration by executing the binary, scrapes their status over
FastCGI, and delivers a `SIGUSR2` reload. The interesting reports are about that
crossing a trust boundary:

- **Execution of an untrusted binary or config.** The binary and config path
  discovered from the process table are checked for ownership before they reach
  exec (`trustedPath`). The process table is deliberately *not* a trust boundary
  — a way past the ownership check, or a config path that reaches a parser
  unchecked, is in scope.
- **Trusting a truncated or forged parse.** A truncated `php-fpm -tt` output is
  refused rather than read as complete, and status output cannot relabel which
  pool it describes. A way to make a partial or forged response look authoritative
  is in scope.
- **Signalling the wrong process.** A reload verifies master identity by start
  time, so a reused PID is not mistaken for the master that was signalled. A way
  to make it signal an unrelated process is in scope.

## What is not

- A caller passing paths or PIDs it should not. The library checks what it
  discovers itself; a caller that hands it an attacker-controlled binary path on
  purpose has already made that decision.

## Supported versions

During the beta, only the latest tagged release is supported. Please reproduce
against it before reporting.
