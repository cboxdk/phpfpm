package phpfpm

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestFmpNamePattern(t *testing.T) {
	// Test the FMP name pattern regex
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{
			name:     "php-fpm",
			input:    "php-fpm",
			expected: true,
		},
		{
			name:     "php8.2-fpm",
			input:    "php8.2-fpm",
			expected: true,
		},
		{
			name:     "php82-fpm",
			input:    "php82-fpm",
			expected: true,
		},
		{
			name:     "phpfpm",
			input:    "phpfpm",
			expected: true,
		},
		{
			name:     "php7.4-fpm",
			input:    "php7.4-fpm",
			expected: true,
		},
		{
			name:     "php-fpm8.1",
			input:    "php-fpm8.1",
			expected: true,
		},
		{
			name:     "php-fpm-custom",
			input:    "php-fpm-custom",
			expected: true,
		},
		{
			name:     "apache2",
			input:    "apache2",
			expected: false,
		},
		{
			name:     "nginx",
			input:    "nginx",
			expected: false,
		},
		{
			name:     "php-cli",
			input:    "php-cli",
			expected: false,
		},
		{
			name:     "mysql",
			input:    "mysql",
			expected: false,
		},
		{
			name:     "empty",
			input:    "",
			expected: false,
		},
		{
			name:     "just php",
			input:    "php",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matches := fpmNamePattern.MatchString(tt.input)
			if matches != tt.expected {
				t.Errorf("Expected fmpNamePattern.MatchString(%q) to be %v, got %v", tt.input, tt.expected, matches)
			}
		})
	}
}

func TestParseSocket(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "unix socket absolute path",
			input:    "/var/run/php-fpm.sock",
			expected: "unix:///var/run/php-fpm.sock",
		},
		{
			name:     "unix socket with subdirectory",
			input:    "/run/php/php8.2-fpm.sock",
			expected: "unix:///run/php/php8.2-fpm.sock",
		},
		{
			name:     "tcp with ip and port",
			input:    "127.0.0.1:9000",
			expected: "tcp://127.0.0.1:9000",
		},
		{
			name:     "tcp with host and port",
			input:    "localhost:9001",
			expected: "tcp://localhost:9001",
		},
		{
			name:     "ipv6 with port",
			input:    "[::1]:9000",
			expected: "tcp://[::1]:9000",
		},
		{
			name:     "empty input",
			input:    "",
			expected: "",
		},
		{
			name:     "just port number (will try to connect)",
			input:    "9000",
			expected: "tcp://127.0.0.1:9000", // May actually connect to localhost in some environments
		},
		{
			name:     "invalid format",
			input:    "invalid-socket",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseSocketT(tt.input)
			if tt.input == "9000" {
				// Port 9000 test is flexible - could connect or not
				if result != "" && result != "tcp://127.0.0.1:9000" && result != "tcp://[::1]:9000" {
					t.Errorf("Expected parseSocketT(%q) to be empty or valid tcp address, got %q", tt.input, result)
				}
			} else if result != tt.expected {
				t.Errorf("Expected parseSocketT(%q) to be %q, got %q", tt.input, tt.expected, result)
			}
		})
	}
}

func TestExtractConfigFromMaster(t *testing.T) {
	tests := []struct {
		name     string
		cmdline  string
		expected string
	}{
		{
			name:     "standard master process",
			cmdline:  "php-fpm: master process (/etc/php/8.2/fpm/php-fpm.conf)",
			expected: "/etc/php/8.2/fpm/php-fpm.conf",
		},
		{
			name:     "custom config path",
			cmdline:  "php-fpm: master process (/custom/path/fpm.conf)",
			expected: "/custom/path/fpm.conf",
		},
		{
			name:     "versioned php-fpm",
			cmdline:  "php-fpm8.1: master process (/etc/php/8.1/fpm/php-fpm.conf)",
			expected: "/etc/php/8.1/fpm/php-fpm.conf",
		},
		{
			name:     "no parentheses",
			cmdline:  "php-fpm: master process",
			expected: "",
		},
		{
			name:     "empty cmdline",
			cmdline:  "",
			expected: "",
		},
		{
			name:     "malformed parentheses - no closing",
			cmdline:  "php-fpm: master process (/etc/php.conf",
			expected: "",
		},
		{
			name:     "malformed parentheses - no opening",
			cmdline:  "php-fpm: master process /etc/php.conf)",
			expected: "",
		},
		{
			name:     "empty parentheses",
			cmdline:  "php-fpm: master process ()",
			expected: "",
		},
		{
			name:     "multiple parentheses - takes first",
			cmdline:  "php-fpm: master process (/etc/php.conf) (extra)",
			expected: "/etc/php.conf",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractConfigFromMaster(tt.cmdline)
			if result != tt.expected {
				t.Errorf("Expected extractConfigFromMaster(%q) to be %q, got %q", tt.cmdline, tt.expected, result)
			}
		})
	}
}

// TestDiscoverFindsARunningMaster.
//
// This replaces a test that skipped itself in CI and, when it did run, asserted
// only that the function returned without panicking — avoiding the one
// environment where it could have been a signal, and proving nothing in the
// others.
//
// With a real php-fpm it can say something: a master that is running is found,
// with the pool it serves and the pid to signal.
func TestDiscoverFindsARunningMaster(t *testing.T) {
	fpm := lookupFPM(t)

	root := t.TempDir()
	pools := filepath.Join(root, "pool.d")
	if err := os.MkdirAll(pools, 0o755); err != nil {
		t.Fatal(err)
	}

	port := 25000 + (os.Getpid() % 10000)
	name := fmt.Sprintf("disco%d", os.Getpid())

	conf := filepath.Join(root, "php-fpm.conf")
	if err := os.WriteFile(conf, fmt.Appendf(nil,
		"[global]\npid = %s/fpm.pid\nerror_log = %s/fpm.log\ndaemonize = yes\ninclude=%s/*.conf\n",
		root, root, pools), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pools, name+".conf"), fmt.Appendf(nil,
		"[%s]\nlisten = 127.0.0.1:%d\npm = dynamic\npm.max_children = 7\n"+
			"pm.start_servers = 2\npm.min_spare_servers = 1\npm.max_spare_servers = 3\n"+
			"pm.status_path = /status\n", name, port), 0o644); err != nil {
		t.Fatal(err)
	}

	if out, err := exec.Command(fpm, "--fpm-config", conf).CombinedOutput(); err != nil {
		t.Skipf("php-fpm would not start here: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		if pid, err := os.ReadFile(filepath.Join(root, "fpm.pid")); err == nil {
			if n, err := strconv.Atoi(strings.TrimSpace(string(pid))); err == nil {
				if p, err := os.FindProcess(n); err == nil {
					_ = p.Signal(syscall.SIGTERM)
				}
			}
		}
	})

	if !waitFor(t, 10*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(root, "fpm.pid"))

		return err == nil
	}) {
		t.Skip("php-fpm wrote no pid file")
	}

	found, err := Discover(nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	for _, d := range found {
		if d.Name != name {
			continue
		}
		if d.PID <= 0 {
			t.Errorf("the pool was found without the pid to signal: %+v", d)
		}
		if d.MaxChildren != 7 {
			t.Errorf("MaxChildren = %d, want the configured 7", d.MaxChildren)
		}
		if d.ConfigPath != conf {
			t.Errorf("ConfigPath = %q, want %q", d.ConfigPath, conf)
		}

		return
	}

	t.Errorf("a running master serving pool %q was not discovered among %d found",
		name, len(found))
}

// TestDiscoverMastersAnswersWhenTheConfigDoesNot is the whole point of the
// split.
//
// Discover parses each master's effective configuration, so a master whose
// config no longer parses is skipped entirely — and a tool trying to REPAIR
// that config then cannot find the master to repair it for. Verified in the
// php:8.3-fpm image: a rejected pool fragment on disk, a healthy master still
// serving what it loaded before the file appeared, and the caller reporting
// "no PHP-FPM pools found" while the fragment waited for any reload to adopt it
// and kill the master permanently.
func TestDiscoverMastersAnswersWhenTheConfigDoesNot(t *testing.T) {
	// Both paths must agree on what a master is, so the identity check is shared
	// rather than duplicated. A config that cannot be parsed is the difference
	// between them and the only difference.
	masters, err := DiscoverMasters(nil)
	if err != nil {
		t.Fatalf("DiscoverMasters: %v", err)
	}

	pools, err := Discover(nil)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	// Every master a pool was found for must also appear in the lighter scan;
	// the reverse need not hold, and that asymmetry is the fix.
	seen := map[int]bool{}
	for _, m := range masters {
		seen[m.PID] = true
	}
	for _, p := range pools {
		if p.PID > 0 && !seen[p.PID] {
			t.Errorf("Discover found a pool on master %d that DiscoverMasters missed; "+
				"the lighter scan must be a superset", p.PID)
		}
	}

	for _, m := range masters {
		if m.Binary == "" || m.ConfigPath == "" || m.PID <= 0 {
			t.Errorf("a master was returned without the identity a caller needs: %+v", m)
		}
	}
}

// TestABarePortIsNotProbedBeforeItIsBelieved.
//
// `listen = 9000` means what PHP-FPM's own documentation says it means, and
// this used to decide by DIALLING — skipping the pool entirely when nothing
// answered.
//
// For a caller that divides one memory budget between the pools it can see,
// that is the worst possible failure: a master still starting up, or a host
// under load, loses pools from discovery, their share goes to the neighbours,
// the neighbours are reloaded with larger ceilings, and the pools come back to
// a host committed past its memory. Whether a socket answers is the scrape's
// question, and the scrape reports it as an unreachable pool rather than as no
// pool at all.
func TestABarePortIsNotProbedBeforeItIsBelieved(t *testing.T) {
	// A port with certainly nothing on it.
	if got := parseSocket("59999", nil); got != "tcp://127.0.0.1:59999" {
		t.Errorf("parseSocket(\"59999\") = %q; a pool whose port is not answering right "+
			"now has vanished from discovery, and its memory with it", got)
	}

	// The forms that were never in doubt.
	for in, want := range map[string]string{
		"/run/php/php-fpm.sock": "unix:///run/php/php-fpm.sock",
		"127.0.0.1:9000":        "tcp://127.0.0.1:9000",
		"[::1]:9000":            "tcp://[::1]:9000",
	} {
		if got := parseSocket(in, nil); got != want {
			t.Errorf("parseSocket(%q) = %q, want %q", in, got, want)
		}
	}

	// And something that is not an address at all still yields nothing.
	if got := parseSocket("not-a-port", nil); got != "" {
		t.Errorf("parseSocket(\"not-a-port\") = %q, want empty", got)
	}
}
