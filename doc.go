// Package phpfpm is a Go library for talking to PHP-FPM.
//
// It covers the four things a caller needs in order to reason about a running
// PHP-FPM installation: parsing the effective configuration, discovering running
// masters and their pools, scraping live status over FastCGI, and controlling
// the master (validate, reload).
//
// It exists as its own module because two consumers need it and neither should
// depend on the other: fpm-exporter reads, and fpm-tune both reads and writes.
// The code began life inside fpm-exporter's internal/phpfpm, where it could not
// be imported.
//
// Two conventions run through the package:
//
// Nothing here owns a logger. Entry points that need one take a *slog.Logger.
// A library with a package-level logger cannot be embedded twice with different
// destinations, and it makes tests order-dependent.
//
// Reload, never restart. Reload sends SIGUSR2, which has the master re-read its
// configuration and cycle workers gracefully. Callers changing pm.* settings
// should pair it with Validate first — an invalid drop-in that reaches a reload
// takes the pool down.
package phpfpm
