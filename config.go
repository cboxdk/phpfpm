package phpfpm

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	fpmConfigCache     = make(map[string]*EffectiveConfig)
	fpmConfigCacheLock sync.Mutex

	// fpmConfigEpoch moves on every invalidation, so a parse that began before
	// one can tell that its result is already out of date.
	fpmConfigEpoch uint64
)

// InvalidateConfigCache forgets the parsed configuration.
//
// Call it after reloading php-fpm with a changed configuration. Without it, a
// long-running process keeps reporting the settings it saw at startup: a tool
// that writes pool settings would never observe its own changes, and would show
// an operator a "currently configured" value that has not been true for hours.
//
// Passing a binary and config path forgets just that pair; passing empty strings
// forgets everything.
func InvalidateConfigCache(binary, configPath string) {
	fpmConfigCacheLock.Lock()
	defer fpmConfigCacheLock.Unlock()

	if binary == "" && configPath == "" {
		clear(fpmConfigCache)
		fpmConfigEpoch++

		return
	}

	delete(fpmConfigCache, cacheKey(binary, configPath))
	fpmConfigEpoch++
}

// cacheKey is how a binary and a config path name one parse.
//
// Built in ONE place and cleaned, because it is used to store and to
// invalidate, and the two were assembled separately from whatever strings the
// caller had. Parsing /etc/php/../php-fpm.conf and invalidating
// /etc/php-fpm.conf are the same file and were two different keys — so the
// invalidation missed, and the stale parse stayed available to every later
// caller with no way left to clear it.
func cacheKey(binary, configPath string) string {
	return filepath.Clean(binary) + "::" + filepath.Clean(configPath)
}

type EffectiveConfig struct {
	Global map[string]string
	Pools  map[string]map[string]string
}

// ParseConfig runs `php-fpm -tt` and parses its report of the effective
// configuration. Results are cached per binary+config pair.
//
// The cache never expires on its own, which is right for the common case — the
// parse forks php-fpm and a scrape loop would otherwise do it every few seconds
// — but it means a caller that CHANGES the configuration has to say so. See
// InvalidateConfigCache.
func ParseConfig(FPMBinaryPath string, FPMConfigPath string) (*EffectiveConfig, error) {
	// A default rather than none. This forks php-fpm, and a fork with no bound is
	// a scrape loop that stops forever the first time the binary wedges — on an
	// NFS-backed include, an operator's wrapper script, a host under memory
	// pressure. Callers that care pass their own.
	ctx, cancel := context.WithTimeout(context.Background(), DefaultParseTimeout)
	defer cancel()

	return ParseConfigContext(ctx, FPMBinaryPath, FPMConfigPath)
}

// DefaultParseTimeout bounds ParseConfig when the caller gives no context.
//
// Generous: `php-fpm -tt` on a host with many pools is not instant, and killing
// a slow-but-working parse is worse than waiting for it. The point is that there
// IS a bound.
const DefaultParseTimeout = 30 * time.Second

// maxParseOutput bounds what is captured from php-fpm.
//
// The output is a configuration dump, which is kilobytes. A process producing
// megabytes is malfunctioning, and reading all of it into memory to report that
// it malfunctioned helps nobody.
const maxParseOutput = 4 << 20

// ParseConfigContext is ParseConfig with a caller-supplied deadline.
func ParseConfigContext(ctx context.Context, FPMBinaryPath string, FPMConfigPath string) (*EffectiveConfig, error) {
	// Same reason as Validate: a relative name goes through PATH, and this forks
	// the result. Discovery supplies an absolute path it has already checked.
	if !filepath.IsAbs(FPMBinaryPath) {
		return nil, fmt.Errorf("php-fpm binary must be an absolute path, got %q", FPMBinaryPath)
	}

	key := cacheKey(FPMBinaryPath, FPMConfigPath)

	fpmConfigCacheLock.Lock()
	cached, ok := fpmConfigCache[key]
	// The epoch this parse is about to be based on. An invalidation while
	// php-fpm is forked below moves it, and the result is then dropped rather
	// than stored: without this a scrape that began before a change could finish
	// after it and repopulate the cache with pre-change data, which no
	// subsequent invalidation would clear because nobody knew it was stale. The
	// pool then reads as configured for what it used to be, so the change is
	// proposed again on every round.
	epoch := fpmConfigEpoch
	fpmConfigCacheLock.Unlock()

	if ok {
		// Copied, not shared. The cache hands the same pointer to every caller
		// for the life of the process, so one caller editing a pool map — or
		// reading it while another edits — poisons every scrape that follows, or
		// panics on Go's map race detection.
		return cached.clone(), nil
	}

	cmd := exec.CommandContext(ctx, FPMBinaryPath, "-tt", "--fpm-config", FPMConfigPath)

	// Its own process group, killed as a group on timeout. Killing only the
	// process leaves anything it started — a wrapper's child, an include that
	// shells out — holding the pipe, so the read would block on a dead parent's
	// descendants.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}

		return os.ErrProcessDone
	}

	var captured bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &captured, remaining: maxParseOutput}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("php-fpm -tt did not finish: %w", ctx.Err())
		}

		return nil, fmt.Errorf("failed to run php-fpm -tt: %w\nOutput: %s",
			err, captured.String())
	}
	if limited, ok := cmd.Stdout.(*limitedWriter); ok && limited.truncated {
		// A PARTIAL configuration is not a configuration. Parsing the prefix
		// gives a clean-looking answer that is missing every pool after the cut,
		// and a caller that writes pool settings will read those pools as
		// removed and take their overrides out.
		return nil, fmt.Errorf("php-fpm -tt produced more than %d bytes of output and the "+
			"configuration would be incomplete; the pools after the cut would look as "+
			"though they had been deleted", maxParseOutput)
	}

	output := captured.Bytes()

	fpmconfig, err := parseFPMConfigOutput(output)
	if err != nil {
		return nil, err
	}

	fpmConfigCacheLock.Lock()
	if fpmConfigEpoch == epoch {
		fpmConfigCache[key] = fpmconfig
	}
	fpmConfigCacheLock.Unlock()

	return fpmconfig.clone(), nil
}

// parseFPMConfigOutput parses the report `php-fpm -tt` writes to stderr. Kept
// free of I/O so it can be driven by a captured fixture: this is the only place
// that knows the shape of that output, and it had no test against real output
// at all.
func parseFPMConfigOutput(output []byte) (*EffectiveConfig, error) {
	fpmconfig := &EffectiveConfig{
		Global: make(map[string]string),
		Pools:  make(map[string]map[string]string),
	}
	currentSection := "global"

	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if idx := strings.Index(line, "] NOTICE:"); idx != -1 {
			line = strings.TrimSpace(line[idx+len("] NOTICE:"):])
		}

		line = strings.ReplaceAll(line, "\\t", "")
		line = strings.ReplaceAll(line, "\t", "")
		line = strings.TrimSpace(strings.Trim(line, `"`))

		if line == "" || strings.HasPrefix(line, ";") {
			continue
		}

		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section := strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			if section == "global" {
				currentSection = "global"
				continue
			}
			currentSection = section
			if _, ok := fpmconfig.Pools[currentSection]; !ok {
				fpmconfig.Pools[currentSection] = make(map[string]string)
			}
			continue
		}

		if strings.Contains(line, "=") {
			parts := strings.SplitN(line, "=", 2)
			// TrimSpace first: the space after `=` used to block the leading
			// quote from being stripped, leaving values like `"50`. php-fpm -tt
			// normalises quotes away so nothing hit it in practice, but the
			// ordering was still wrong -- and the test accepted either result.
			key := strings.Trim(strings.TrimSpace(parts[0]), `"`)
			val := strings.Trim(strings.TrimSpace(parts[1]), `"`)
			if val == "undefined" {
				val = ""
			}

			if currentSection != "global" {
				if _, ok := fpmconfig.Pools[currentSection]; !ok {
					fpmconfig.Pools[currentSection] = make(map[string]string)
				}
				fpmconfig.Pools[currentSection][key] = val
			} else {
				fpmconfig.Global[key] = val
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to scan php-fpm config output: %w", err)
	}

	return fpmconfig, nil
}

// limitedWriter stops accepting once it has taken enough.
//
// Discards rather than erroring: a process that overruns is malfunctioning, and
// the useful thing to report is what it said first, not that we gave up reading.
// limitedWriter caps what is captured, and REMEMBERS that it did.
//
// Silently discarding the overflow was worse than the unbounded read it
// replaced. `php-fpm -tt` prints the effective configuration in include order,
// so a truncated capture parses cleanly into a config that is missing every
// pool after the cut — and a caller that writes configuration then treats those
// pools as gone. On a shared host one tenant with a few thousand env lines in
// their own pool file can push everybody after them past the cap.
type limitedWriter struct {
	w         *bytes.Buffer
	remaining int
	truncated bool
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.remaining <= 0 {
		l.truncated = true

		return len(p), nil
	}
	if len(p) > l.remaining {
		p = p[:l.remaining]
		l.truncated = true
	}
	n, err := l.w.Write(p)
	l.remaining -= n

	return len(p), err
}

// clone returns a copy safe for a caller to keep or modify.
func (c *EffectiveConfig) clone() *EffectiveConfig {
	if c == nil {
		return nil
	}

	out := &EffectiveConfig{
		Global: make(map[string]string, len(c.Global)),
		Pools:  make(map[string]map[string]string, len(c.Pools)),
	}
	for k, v := range c.Global {
		out.Global[k] = v
	}
	for name, pool := range c.Pools {
		copied := make(map[string]string, len(pool))
		for k, v := range pool {
			copied[k] = v
		}
		out.Pools[name] = copied
	}

	return out
}
