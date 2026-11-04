package phpfpm

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/cboxdk/fcgx"
)

// ReadStatus reads only the native FPM full-status page. It does not execute
// PHP-info/OPcache scripts, inspect OS processes or invoke an FPM binary.
// Counters and process states are returned as reported, including the worker
// handling this status request. A successful read is a point-in-time observation,
// not a guarantee that no new work can start or that all pools are idle.
func ReadStatus(ctx context.Context, target Target) (Pool, error) {
	return readStatus(ctx, target, logOrDiscard(nil))
}

func readStatus(ctx context.Context, target Target, log *slog.Logger) (Pool, error) {
	scheme, address, path, err := ParseAddress(target.statusAddress(), target.StatusPath)
	if err != nil {
		return Pool{}, fmt.Errorf("invalid FPM socket address: %w", err)
	}

	// The timeout bounds the whole exchange, not just the dial. It used to cover
	// the connect and then hand the parent context — often context.Background()
	// — to the request, so a server that accepted the socket and never sent
	// END_REQUEST hung the scrape indefinitely. A pool being scraped is a pool
	// that is unwell often enough for that to matter.
	ctx, cancel := context.WithTimeout(ctx, target.dialTimeout())
	defer cancel()

	log.Debug("Dialing FastCGI", "scheme", scheme, "address", address, "status_path", path)
	client, err := fcgx.DialContext(ctx, scheme, address)
	if err != nil {
		return Pool{}, fmt.Errorf("failed to dial FastCGI: %w", err)
	}
	defer func() { _ = client.Close() }()

	env := map[string]string{
		"SCRIPT_FILENAME": path,
		"SCRIPT_NAME":     path,
		"SERVER_SOFTWARE": serverSoftware,
		"SERVER_PROTOCOL": "HTTP/1.1",
		"REMOTE_ADDR":     "127.0.0.1",
		"QUERY_STRING":    "json&full",
	}
	log.Debug("Sending FCGI request", "env", env)

	resp, err := client.Get(ctx, env)
	if err != nil {
		return Pool{}, fmt.Errorf("fcgi GET failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Pointers distinguish a measured zero from an omitted/null field. Lifecycle
	// callers must never interpret an unavailable queue or worker list as idle.
	var payload struct {
		Pool
		Active  *int64         `json:"active processes"`
		Idle    *int64         `json:"idle processes"`
		Total   *int64         `json:"total processes"`
		Queue   *int64         `json:"listen queue"`
		Workers *[]PoolProcess `json:"processes"`
	}
	if err := fcgx.ReadJSON(resp, &payload); err != nil {
		return Pool{}, fmt.Errorf("failed to parse FPM status JSON: %w", err)
	}

	if payload.Active == nil || payload.Idle == nil || payload.Total == nil || payload.Queue == nil || payload.Workers == nil {
		return Pool{}, fmt.Errorf("FPM full status is missing required worker or queue fields")
	}
	pool := payload.Pool
	pool.ActiveProcesses, pool.IdleProcesses = *payload.Active, *payload.Idle
	pool.TotalProcesses, pool.ListenQueue = *payload.Total, *payload.Queue
	pool.Processes = *payload.Workers
	if err := checkIsStatusFor(pool.Name, target.Name, path); err != nil {
		return Pool{}, err
	}

	pool.Address = address
	pool.Path = path
	return pool, nil
}
