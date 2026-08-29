package phpfpm

import (
	"context"
	"strings"
	"sync"
	"time"
)

// phpInfoTTL bounds how long a binary's version and extension list are reused.
// Failures are cached for a shorter window: without recording the timestamp on
// the failure path, a binary that errors re-forked `php -v` for every pool on
// every scrape, forever.
const (
	phpInfoTTL        = time.Hour
	phpInfoFailureTTL = time.Minute
)

type phpInfoEntry struct {
	info      *Info
	err       error
	expiresAt time.Time
}

// phpInfoCall is one in-flight lookup that later callers wait on instead of
// repeating.
type phpInfoCall struct {
	done chan struct{}
	info *Info
	err  error
}

// Keyed by binary path. A single unkeyed global meant that on a host running
// php8.1-fpm and php8.3-fpm side by side, whichever pool was scraped first
// decided the version every other pool reported for the next hour.
var (
	phpInfoMu       sync.Mutex
	phpInfoCache    = map[string]phpInfoEntry{}
	phpInfoInFlight = map[string]*phpInfoCall{}
)

type Info struct {
	Version    string
	Extensions []string
}

// clone returns a value no other caller holds a reference into. Nil stays nil,
// so an error result is still distinguishable from an empty one.
func (i *Info) clone() *Info {
	if i == nil {
		return nil
	}

	out := &Info{Version: i.Version}
	if i.Extensions != nil {
		out.Extensions = append([]string(nil), i.Extensions...)
	}

	return out
}

// resetPHPInfoCache clears the cache. Used by tests, which would otherwise
// inherit whatever a previous test resolved.
func resetPHPInfoCache() {
	phpInfoMu.Lock()
	defer phpInfoMu.Unlock()
	phpInfoCache = map[string]phpInfoEntry{}
	phpInfoInFlight = map[string]*phpInfoCall{}
}

func GetPHPStats(ctx context.Context, target Target) (*Info, error) {
	phpInfoMu.Lock()

	if entry, ok := phpInfoCache[target.Binary]; ok && time.Now().Before(entry.expiresAt) {
		phpInfoMu.Unlock()

		// A COPY. The cached value is shared by every caller for this binary,
		// and Info carries a slice — one caller sorting or appending to
		// Extensions changes what every other caller sees, from a package that
		// looks read-only.
		return entry.info.clone(), entry.err
	}

	// A cold cache with N pools sharing a binary should fork once, not N times
	// -- but the fork must not happen under the lock, because holding a
	// package-global mutex across an exec meant one hung binary blocked every
	// later scrape forever. So callers coalesce onto the first one's result.
	if call, ok := phpInfoInFlight[target.Binary]; ok {
		phpInfoMu.Unlock()

		// Waited for, but not indefinitely. Coalescing onto the first caller's
		// fork is right; inheriting the first caller's PATIENCE is not — a
		// scrape with a two-second budget sat behind a wedged binary until
		// somebody else's timeout expired, and the deadline it was given bought
		// it nothing.
		select {
		case <-call.done:
			return call.info.clone(), call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	call := &phpInfoCall{done: make(chan struct{})}
	phpInfoInFlight[target.Binary] = call
	phpInfoMu.Unlock()

	call.info, call.err = readPHPInfo(ctx, target.Binary)

	ttl := phpInfoTTL
	if call.err != nil {
		ttl = phpInfoFailureTTL
	}

	phpInfoMu.Lock()
	phpInfoCache[target.Binary] = phpInfoEntry{info: call.info, err: call.err, expiresAt: time.Now().Add(ttl)}
	delete(phpInfoInFlight, target.Binary)
	phpInfoMu.Unlock()

	close(call.done)

	return call.info.clone(), call.err
}

func readPHPInfo(ctx context.Context, binary string) (*Info, error) {
	version, err := getPHPVersion(ctx, binary)
	if err != nil {
		return nil, err
	}

	ext, err := getPHPExtensions(ctx, binary)
	if err != nil {
		return nil, err
	}

	return &Info{Version: version, Extensions: ext}, nil
}

func getPHPVersion(ctx context.Context, bin string) (string, error) {
	out, err := runBounded(ctx, bin, "-v")
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(out), "\n")
	if len(lines) > 0 {
		return strings.TrimSpace(lines[0]), nil
	}
	return "unknown", nil
}

func getPHPExtensions(ctx context.Context, bin string) ([]string, error) {
	out, err := runBounded(ctx, bin, "-m")
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(out), "\n")
	var exts []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "[") {
			exts = append(exts, line)
		}
	}
	return exts, nil
}
