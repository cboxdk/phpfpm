package phpfpm

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseFPMConfig_MockOutput(t *testing.T) {
	// Create a mock php-fpm binary that outputs test configuration
	tempDir := t.TempDir()
	mockFpmPath := tempDir + "/mock-php-fpm"
	configPath := tempDir + "/test-fpm.conf"

	// Create mock php-fpm script
	mockScript := `#!/bin/bash
cat << 'EOF'
[global]
pid = /var/run/php-fpm.pid
error_log = /var/log/php-fpm.log
daemonize = yes

[www]
user = www-data
group = www-data
listen = /var/run/php-fpm.sock
pm = dynamic
pm.max_children = 50
pm.start_servers = 5
pm.min_spare_servers = 5
pm.max_spare_servers = 35
pm.status_path = /status

[api]
user = api-user
group = api-group
listen = 127.0.0.1:9001
pm = static
pm.max_children = 20
pm.status_path = /api-status
EOF`

	err := os.WriteFile(mockFpmPath, []byte(mockScript), 0755)
	if err != nil {
		t.Fatalf("Failed to create mock php-fpm script: %v", err)
	}

	// Clear cache to ensure fresh parsing
	fpmConfigCacheLock.Lock()
	fpmConfigCache = make(map[string]*EffectiveConfig)
	fpmConfigCacheLock.Unlock()

	// Test parsing
	config, err := ParseConfig(mockFpmPath, configPath)
	if err != nil {
		t.Fatalf("ParseFPMConfig failed: %v", err)
	}

	// Verify global settings
	expectedGlobal := map[string]string{
		"pid":       "/var/run/php-fpm.pid",
		"error_log": "/var/log/php-fpm.log",
		"daemonize": "yes",
	}

	for key, expectedValue := range expectedGlobal {
		if config.Global[key] != expectedValue {
			t.Errorf("Expected global[%s] to be '%s', got '%s'", key, expectedValue, config.Global[key])
		}
	}

	// Verify pools
	if len(config.Pools) != 2 {
		t.Errorf("Expected 2 pools, got %d", len(config.Pools))
	}

	// Verify www pool
	wwwPool, exists := config.Pools["www"]
	if !exists {
		t.Fatalf("Expected 'www' pool to exist")
	}

	expectedWww := map[string]string{
		"user":                 "www-data",
		"group":                "www-data",
		"listen":               "/var/run/php-fpm.sock",
		"pm":                   "dynamic",
		"pm.max_children":      "50",
		"pm.start_servers":     "5",
		"pm.min_spare_servers": "5",
		"pm.max_spare_servers": "35",
		"pm.status_path":       "/status",
	}

	for key, expectedValue := range expectedWww {
		if wwwPool[key] != expectedValue {
			t.Errorf("Expected www[%s] to be '%s', got '%s'", key, expectedValue, wwwPool[key])
		}
	}

	// Verify api pool
	apiPool, exists := config.Pools["api"]
	if !exists {
		t.Fatalf("Expected 'api' pool to exist")
	}

	expectedApi := map[string]string{
		"user":            "api-user",
		"group":           "api-group",
		"listen":          "127.0.0.1:9001",
		"pm":              "static",
		"pm.max_children": "20",
		"pm.status_path":  "/api-status",
	}

	for key, expectedValue := range expectedApi {
		if apiPool[key] != expectedValue {
			t.Errorf("Expected api[%s] to be '%s', got '%s'", key, expectedValue, apiPool[key])
		}
	}
}

// TestParseFPMConfig_Caching proves what the cache is FOR — not re-forking
// php-fpm on every scrape — rather than that every caller gets the same pointer.
//
// It used to assert pointer identity, which is an implementation detail and an
// unsafe one: the same maps handed to every caller for the life of the process
// means one caller editing a pool poisons every scrape that follows, and two
// callers touching it at once panic on Go's map race detection. The test was
// holding that in place.
func TestParseFPMConfig_Caching(t *testing.T) {
	tempDir := t.TempDir()
	mockFpmPath := tempDir + "/mock-php-fpm-cache"
	configPath := tempDir + "/test-cache.conf"
	forks := tempDir + "/forks"

	// Records every invocation, so "was it cached" is measured rather than
	// inferred from an address.
	mockScript := `#!/bin/bash
echo x >> ` + forks + `
echo "[global]"
echo "pid = /test/cache.pid"
echo "[test]"
echo "listen = /test/cache.sock"
`

	if err := os.WriteFile(mockFpmPath, []byte(mockScript), 0o755); err != nil {
		t.Fatalf("Failed to create mock script: %v", err)
	}

	fpmConfigCacheLock.Lock()
	fpmConfigCache = make(map[string]*EffectiveConfig)
	fpmConfigCacheLock.Unlock()

	config1, err := ParseConfig(mockFpmPath, configPath)
	if err != nil {
		t.Fatalf("First ParseFPMConfig failed: %v", err)
	}

	config2, err := ParseConfig(mockFpmPath, configPath)
	if err != nil {
		t.Fatalf("Second ParseFPMConfig failed: %v", err)
	}

	body, err := os.ReadFile(forks)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(body), "x"); n != 1 {
		t.Errorf("php-fpm was forked %d times for two calls; the cache is not being used", n)
	}

	if len(config2.Pools) != len(config1.Pools) {
		t.Errorf("the cached result differs from the first: %v vs %v", config2.Pools, config1.Pools)
	}

	// Independent copies. A caller that edits what it was handed must not change
	// what the next caller sees.
	config1.Pools["test"]["listen"] = "/somewhere/else.sock"

	config3, err := ParseConfig(mockFpmPath, configPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := config3.Pools["test"]["listen"]; got != "/test/cache.sock" {
		t.Errorf("one caller's edit reached the next: listen = %q", got)
	}

	fpmConfigCacheLock.Lock()
	_, exists := fpmConfigCache[mockFpmPath+"::"+configPath]
	fpmConfigCacheLock.Unlock()
	if !exists {
		t.Error("nothing was cached")
	}
}

func TestParseFPMConfig_ErrorHandling(t *testing.T) {
	// Test with non-existent binary
	_, err := ParseConfig("/non/existent/php-fpm", "/non/existent/config.conf")
	if err == nil {
		t.Errorf("Expected error for non-existent binary")
	}

	if !strings.Contains(err.Error(), "failed to run php-fpm -tt") {
		t.Errorf("Expected error message to mention 'failed to run php-fpm -tt', got: %s", err.Error())
	}
}

func TestParseFPMConfig_ComplexOutput(t *testing.T) {
	// Test parsing with NOTICE prefixes and various formatting
	tempDir := t.TempDir()
	mockFpmPath := tempDir + "/mock-php-fpm-complex"
	configPath := tempDir + "/complex.conf"

	mockScript := `#!/bin/bash
cat << 'EOF'
[12-Dec-2023 10:30:45] NOTICE: [global]
[12-Dec-2023 10:30:45] NOTICE: pid = "/var/run/php-fpm.pid"
[12-Dec-2023 10:30:45] NOTICE: error_log = "/var/log/php-fpm.log"
[12-Dec-2023 10:30:45] NOTICE: 
[12-Dec-2023 10:30:45] NOTICE: ; This is a comment
[12-Dec-2023 10:30:45] NOTICE: daemonize = "yes"
[12-Dec-2023 10:30:45] NOTICE: 
[12-Dec-2023 10:30:45] NOTICE: [www]
[12-Dec-2023 10:30:45] NOTICE: user = "www-data"
[12-Dec-2023 10:30:45] NOTICE: group = "www-data"  
[12-Dec-2023 10:30:45] NOTICE: listen = "/var/run/php-fpm.sock"
[12-Dec-2023 10:30:45] NOTICE: undefined_value = undefined
[12-Dec-2023 10:30:45] NOTICE: pm.status_path = "/status"
EOF`

	err := os.WriteFile(mockFpmPath, []byte(mockScript), 0755)
	if err != nil {
		t.Fatalf("Failed to create mock script: %v", err)
	}

	// Clear cache
	fpmConfigCacheLock.Lock()
	fpmConfigCache = make(map[string]*EffectiveConfig)
	fpmConfigCacheLock.Unlock()

	config, err := ParseConfig(mockFpmPath, configPath)
	if err != nil {
		t.Fatalf("ParseFPMConfig failed: %v", err)
	}

	if config.Global["pid"] != "/var/run/php-fpm.pid" {
		t.Errorf("Expected pid to be '/var/run/php-fpm.pid', got '%s'", config.Global["pid"])
	}

	if config.Global["error_log"] != "/var/log/php-fpm.log" {
		t.Errorf("Expected error_log to be '/var/log/php-fpm.log', got '%s'", config.Global["error_log"])
	}

	if config.Global["daemonize"] != "yes" {
		t.Errorf("Expected daemonize to be 'yes', got '%s'", config.Global["daemonize"])
	}

	// Test pool parsing
	wwwPool, exists := config.Pools["www"]
	if !exists {
		t.Fatalf("Expected 'www' pool to exist")
	}

	if wwwPool["user"] != "www-data" {
		t.Errorf("Expected user to be 'www-data', got '%s'", wwwPool["user"])
	}

	// Test that undefined values become empty strings
	if wwwPool["undefined_value"] != "" {
		t.Errorf("Expected undefined_value to be empty string, got '%s'", wwwPool["undefined_value"])
	}

	if wwwPool["pm.status_path"] != "/status" {
		t.Errorf("Expected pm.status_path to be '/status', got '%s'", wwwPool["pm.status_path"])
	}
}

func TestParseFPMConfig_EmptyOutput(t *testing.T) {
	// Test with empty output
	tempDir := t.TempDir()
	mockFpmPath := tempDir + "/mock-php-fpm-empty"
	configPath := tempDir + "/empty.conf"

	mockScript := `#!/bin/bash
# Output nothing
exit 0`

	err := os.WriteFile(mockFpmPath, []byte(mockScript), 0755)
	if err != nil {
		t.Fatalf("Failed to create mock script: %v", err)
	}

	// Clear cache
	fpmConfigCacheLock.Lock()
	fpmConfigCache = make(map[string]*EffectiveConfig)
	fpmConfigCacheLock.Unlock()

	config, err := ParseConfig(mockFpmPath, configPath)
	if err != nil {
		t.Fatalf("ParseFPMConfig failed: %v", err)
	}

	// Should have empty maps
	if len(config.Global) != 0 {
		t.Errorf("Expected empty Global map, got %d entries", len(config.Global))
	}

	if len(config.Pools) != 0 {
		t.Errorf("Expected empty Pools map, got %d entries", len(config.Pools))
	}
}

func TestFPMConfig_ConcurrentAccess(t *testing.T) {
	// Test concurrent access to cache
	tempDir := t.TempDir()
	mockFpmPath := tempDir + "/mock-php-fpm-concurrent"
	configPath := tempDir + "/concurrent.conf"

	mockScript := `#!/bin/bash
echo "[global]"
echo "pid = /test/concurrent.pid"
echo "[pool1]"
echo "listen = /test/pool1.sock"
sleep 0.1
`

	err := os.WriteFile(mockFpmPath, []byte(mockScript), 0755)
	if err != nil {
		t.Fatalf("Failed to create mock script: %v", err)
	}

	// Clear cache
	fpmConfigCacheLock.Lock()
	fpmConfigCache = make(map[string]*EffectiveConfig)
	fpmConfigCacheLock.Unlock()

	// Launch multiple goroutines to test concurrent access
	results := make(chan *EffectiveConfig, 5)
	errors := make(chan error, 5)

	for i := 0; i < 5; i++ {
		go func() {
			config, err := ParseConfig(mockFpmPath, configPath)
			if err != nil {
				errors <- err
				return
			}
			results <- config
		}()
	}

	// Collect results
	var configs []*EffectiveConfig
	for i := 0; i < 5; i++ {
		select {
		case config := <-results:
			configs = append(configs, config)
		case err := <-errors:
			t.Fatalf("Concurrent ParseFPMConfig failed: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatalf("Timeout waiting for concurrent ParseFPMConfig")
		}
	}

	// In concurrent scenarios, we might get different instances if they race
	// Just verify we got valid configs and no panics occurred
	for i, config := range configs {
		if config == nil {
			t.Fatalf("Expected config %d to be non-nil", i)
		}
		if len(config.Global) == 0 && len(config.Pools) == 0 {
			t.Errorf("Expected config %d to have some content", i)
		}
	}
}

// `php-fpm -tt` dumps the effective configuration, which routinely carries
// secrets. The whole map used to be attached to the pool and serialised on the
// unauthenticated /json endpoint.
func TestExportableConfigDropsEverythingNotPublished(t *testing.T) {
	values := map[string]string{
		"pm.max_children":            "50",
		"request_terminate_timeout":  "120s",
		"rlimit_files":               "1024",
		"env[DB_PASSWORD]":           "sup3rs3cret",
		"env[APP_KEY]":               "base64:secret",
		"php_admin_value[error_log]": "/var/log/app.log",
		"listen.acl_users":           "www-data",
		"user":                       "www-data",
		"chdir":                      "/var/www",
		"access.log":                 "/var/log/access.log",
	}

	exported := exportableConfig(values)

	for _, key := range []string{"pm.max_children", "request_terminate_timeout", "rlimit_files"} {
		if _, ok := exported[key]; !ok {
			t.Errorf("Expected %s to be exported: it is emitted as a metric", key)
		}
	}

	for key, value := range exported {
		if !exportedConfigKeys[key] {
			t.Errorf("Exported an unpublished setting: %s = %s", key, value)
		}
	}

	if len(exported) != 3 {
		t.Errorf("Expected exactly the 3 published keys, got %d: %v", len(exported), exported)
	}
}

// TestInvalidateConfigCache: the cache never expires on its own, which is right
// for a scrape loop — the parse forks php-fpm — but wrong for a caller that
// changes the configuration. Without invalidation such a caller never observes
// its own writes, and reports a "currently configured" value that stopped being
// true hours ago.
func TestInvalidateConfigCache(t *testing.T) {
	// Seed the cache directly: the point under test is the eviction, not the
	// parse, and forking php-fpm here would make this a different test.
	fpmConfigCacheLock.Lock()
	fpmConfigCache["/usr/sbin/php-fpm::/etc/a.conf"] = &EffectiveConfig{Global: map[string]string{"x": "1"}}
	fpmConfigCache["/usr/sbin/php-fpm::/etc/b.conf"] = &EffectiveConfig{Global: map[string]string{"x": "2"}}
	fpmConfigCacheLock.Unlock()

	t.Cleanup(func() { InvalidateConfigCache("", "") })

	InvalidateConfigCache("/usr/sbin/php-fpm", "/etc/a.conf")

	fpmConfigCacheLock.Lock()
	_, aStill := fpmConfigCache["/usr/sbin/php-fpm::/etc/a.conf"]
	_, bStill := fpmConfigCache["/usr/sbin/php-fpm::/etc/b.conf"]
	fpmConfigCacheLock.Unlock()

	if aStill {
		t.Error("the named entry was not evicted")
	}
	if !bStill {
		t.Error("an unrelated entry was evicted")
	}

	InvalidateConfigCache("", "")

	fpmConfigCacheLock.Lock()
	remaining := len(fpmConfigCache)
	fpmConfigCacheLock.Unlock()

	if remaining != 0 {
		t.Errorf("%d entries survived a full invalidation", remaining)
	}
}

// TestParseConfigDoesNotHangForever.
//
// ParseConfig forks php-fpm, and a fork with no bound is a scrape loop that
// stops forever the first time the binary wedges — an NFS-backed include, an
// operator's wrapper script, a host under memory pressure. The consumer is a
// daemon that must keep observing whatever else is wrong.
func TestParseConfigDoesNotHangForever(t *testing.T) {
	// A "php-fpm" that never returns.
	dir := t.TempDir()
	stub := filepath.Join(dir, "php-fpm")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nsleep 300\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := ParseConfigContext(ctx, stub, filepath.Join(dir, "php-fpm.conf"))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a php-fpm that never returns was reported as a successful parse")
	}
	if elapsed > 10*time.Second {
		t.Errorf("took %s to give up; the loop above this is stopped for that long", elapsed)
	}
}

// TestParseConfigDoesNotSwallowUnboundedOutput: the output is a configuration
// dump, which is kilobytes. Reading megabytes into memory to report that a
// process is malfunctioning helps nobody.
func TestParseConfigDoesNotSwallowUnboundedOutput(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "php-fpm")
	// Writes far more than the cap, then fails.
	script := "#!/bin/sh\nfor i in $(seq 1 200); do head -c 65536 /dev/zero | tr '\\0' 'x'; done\nexit 78\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, err := ParseConfigContext(ctx, stub, filepath.Join(dir, "php-fpm.conf"))
	if err == nil {
		t.Fatal("a failing php-fpm was reported as a successful parse")
	}
	if len(err.Error()) > 8<<20 {
		t.Errorf("the error carries %d bytes of output; it was not bounded", len(err.Error()))
	}
}

// TestTheCacheKeyIsTheFileNotTheSpelling.
//
// The key was assembled separately where a parse is stored and where one is
// invalidated, from whatever strings the caller happened to have. So parsing
// /etc/php/../php-fpm.conf and invalidating /etc/php-fpm.conf — the same file —
// were two different keys: the invalidation missed, and the stale parse stayed
// available to every later caller with nothing left that could clear it.
//
// For a caller that writes pool configuration, a stale parse is the wrong
// pm.max_children, believed indefinitely.
func TestTheCacheKeyIsTheFileNotTheSpelling(t *testing.T) {
	for _, tc := range []struct{ a, b [2]string }{
		{[2]string{"/usr/sbin/php-fpm", "/etc/php/../php-fpm.conf"},
			[2]string{"/usr/sbin/php-fpm", "/etc/php-fpm.conf"}},
		{[2]string{"/usr/sbin/./php-fpm", "/etc/php-fpm.conf"},
			[2]string{"/usr/sbin/php-fpm", "/etc/php-fpm.conf"}},
		{[2]string{"/usr/sbin/php-fpm", "/etc//php-fpm.conf"},
			[2]string{"/usr/sbin/php-fpm", "/etc/php-fpm.conf"}},
	} {
		if got, want := cacheKey(tc.a[0], tc.a[1]), cacheKey(tc.b[0], tc.b[1]); got != want {
			t.Errorf("%q keys as %q and %q keys as %q; an invalidation of one leaves the "+
				"other's parse in the cache for ever", tc.a, got, tc.b, want)
		}
	}
}

// TestATruncatedParseIsNotAConfiguration.
//
// `php-fpm -tt` prints the effective configuration in include order, and the
// capture is capped so a binary that will not stop talking cannot fill memory.
// The overflow was discarded silently, which is worse than the unbounded read
// it replaced: the prefix parses cleanly into a configuration that is missing
// every pool after the cut.
//
// A caller that writes pool settings then reads those pools as REMOVED and
// takes their overrides out — so a site held at 8 workers by this tooling jumps
// back to its own pm.max_children on the next reload. On a shared host, one
// tenant with a few thousand env lines in their own pool file pushes everybody
// after them past the cap.
func TestATruncatedParseIsNotAConfiguration(t *testing.T) {
	var b bytes.Buffer
	w := &limitedWriter{w: &b, remaining: 16}

	if _, err := w.Write([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	if w.truncated {
		t.Fatal("a write inside the cap was reported as truncated")
	}

	if _, err := w.Write([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	if !w.truncated {
		t.Error("a write past the cap was not recorded; the parse that follows looks " +
			"complete and is missing every pool after the cut")
	}

	// And once past it, further writes are still remembered as truncation
	// rather than passing silently.
	w2 := &limitedWriter{w: &bytes.Buffer{}, remaining: 0}
	if _, err := w2.Write([]byte("anything")); err != nil {
		t.Fatal(err)
	}
	if !w2.truncated {
		t.Error("a write to a full buffer was discarded without a trace")
	}
}
