# Changelog

## [1.3.0] - 2026-11-04

Release candidate; publication requires operator approval.

### Added

- `ReadStatus(ctx, target)` reads native FPM status without PHP enrichment,
  process discovery or OPcache probing.
- `Pool.OpcacheObserved` distinguishes an explicit cache observation from an
  unavailable probe. Consumers must not infer disabled state from an unavailable
  observation.

### Fixed

- Preserve native OPcache `start_time` and `last_restart_time` through JSON
  decoding and encoding. Missing epochs remain absent rather than becoming zero.
- Reject native status responses missing queue or worker fields instead of
  treating missing measurements as observed zeroes.
- Reject null or malformed OPcache state while preserving an explicit disabled
  state as a valid observation.

### Changed

- Require the Go 1.26.10 toolchain floor used by CI.

[1.3.0]: https://github.com/cboxdk/phpfpm/compare/v1.2.0...main
