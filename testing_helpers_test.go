package phpfpm

// parseSocketT calls parseSocket with no logger. parseSocket gained a
// *slog.Logger parameter during the extraction — the version in fpm-exporter
// reached for a package-level global — and the tests care about the returned
// address, not the logging. Passing nil is deliberate: it is also the
// regression test for parseSocket normalising its own logger.
func parseSocketT(socket string) string {
	return parseSocket(socket, nil)
}
