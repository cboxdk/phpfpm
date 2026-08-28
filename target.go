package phpfpm

import (
	"log/slog"
	"time"
)

// Target describes how to reach one PHP-FPM pool.
//
// It replaces the FPMPoolConfig this package used to take from fpm-exporter's
// internal config. A library cannot depend on one application's configuration
// type — the second consumer has a different one, and neither should have to
// adopt the other's.
//
// The mapstructure tags are kept so a viper-based caller can decode straight
// into this type rather than maintaining a parallel struct and a copy function.
type Target struct {
	// Name identifies the pool in results when the pool itself could not be
	// reached. A successful scrape uses the name PHP-FPM reports; this is the
	// fallback so a failing pool is still labelled with something meaningful.
	Name string `mapstructure:"name"`

	// Socket is the pool's FastCGI address ("unix:///run/php-fpm.sock",
	// "tcp://127.0.0.1:9000", or a bare path).
	Socket string `mapstructure:"socket"`

	// StatusSocket is where the status page is served, when it differs from
	// Socket. Empty means Socket.
	StatusSocket string `mapstructure:"status_socket"`
	StatusPath   string `mapstructure:"status_path"`

	// ConfigPath and Binary let ParseConfig recover the effective configuration
	// for this pool. Both come from the master's command line during Discover.
	ConfigPath string `mapstructure:"config_path"`
	Binary     string `mapstructure:"binary"`

	// PID is the master serving this pool, when it is known. Zero means unknown,
	// not "no master".
	PID int `mapstructure:"-"`

	// Timeout bounds the FastCGI dial for this pool. Zero means DefaultTimeout.
	Timeout time.Duration `mapstructure:"timeout"`
}

// DefaultTimeout bounds a FastCGI dial when a Target does not set one.
const DefaultTimeout = 3 * time.Second

// dialTimeout is the timeout actually used for this target.
func (t Target) dialTimeout() time.Duration {
	if t.Timeout > 0 {
		return t.Timeout
	}

	return DefaultTimeout
}

// statusAddress is where the status page lives: StatusSocket when set, and
// otherwise the pool's own socket.
func (t Target) statusAddress() string {
	if t.StatusSocket != "" {
		return t.StatusSocket
	}

	return t.Socket
}

// label prefers the configured name and falls back to the socket, so a pool that
// fails before PHP-FPM ever answers still carries something usable.
func (t Target) label() string {
	if t.Name != "" {
		return t.Name
	}
	if t.StatusSocket != "" {
		return t.StatusSocket
	}

	return t.Socket
}

// TargetFromDiscovered builds a scrape target from a discovered pool.
func TargetFromDiscovered(d Discovered) Target {
	return Target{
		Name:         d.Name,
		Socket:       d.Socket,
		StatusSocket: d.StatusSocket,
		StatusPath:   d.StatusPath,
		ConfigPath:   d.ConfigPath,
		Binary:       d.Binary,
		PID:          d.PID,
	}
}

// logOrDiscard returns a usable logger for a caller that passed nil.
//
// This package deliberately has no package-level logger. The version inside
// fpm-exporter called a global logging.L(), which cannot be embedded twice with
// different destinations and makes tests order-dependent.
func logOrDiscard(l *slog.Logger) *slog.Logger {
	if l == nil {
		return slog.New(slog.DiscardHandler)
	}

	return l
}
