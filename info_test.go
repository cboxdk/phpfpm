package phpfpm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGetPHPStats_MockBinary(t *testing.T) {

	// Create mock PHP binary
	tempDir := t.TempDir()
	mockPhpPath := tempDir + "/mock-php"

	// Create mock PHP script that responds to both -v and -m
	mockScript := `#!/bin/bash
if [[ "$1" == "-v" ]]; then
    echo "PHP 8.2.10 (cli) (built: Sep  1 2023 10:30:45)"
    echo "Copyright (c) The PHP Group"
    echo "Zend Engine v4.2.10, Copyright (c) Zend Technologies"
elif [[ "$1" == "-m" ]]; then
    echo "[PHP Modules]"
    echo "Core"
    echo "date"
    echo "filter"
    echo "hash"
    echo "json"
    echo "pcre"
    echo "Reflection"
    echo "SPL"
    echo ""
    echo "[Zend Modules]"
    echo "Zend OPcache"
fi
`

	err := os.WriteFile(mockPhpPath, []byte(mockScript), 0755)
	if err != nil {
		t.Fatalf("Failed to create mock PHP binary: %v", err)
	}

	// Clear cache to ensure fresh call
	resetPHPInfoCache()

	// Create test config
	cfg := Target{
		Binary: mockPhpPath,
	}

	ctx := context.Background()
	info, err := GetPHPStats(ctx, cfg)
	if err != nil {
		t.Fatalf("GetPHPStats failed: %v", err)
	}

	// Verify version
	expectedVersion := "PHP 8.2.10 (cli) (built: Sep  1 2023 10:30:45)"
	if info.Version != expectedVersion {
		t.Errorf("Expected version '%s', got '%s'", expectedVersion, info.Version)
	}

	// Verify extensions (may have an extra empty line)
	expectedExtensions := []string{"Core", "date", "filter", "hash", "json", "pcre", "Reflection", "SPL"}
	if len(info.Extensions) < len(expectedExtensions) {
		t.Errorf("Expected at least %d extensions, got %d", len(expectedExtensions), len(info.Extensions))
	}

	for i, expected := range expectedExtensions {
		if i >= len(info.Extensions) || info.Extensions[i] != expected {
			t.Errorf("Expected extension[%d] to be '%s', got '%s'", i, expected, info.Extensions[i])
		}
	}
}

func TestGetPHPStats_Caching(t *testing.T) {

	// Create mock PHP binary
	tempDir := t.TempDir()
	mockPhpPath := tempDir + "/mock-php-cache"

	mockScript := `#!/bin/bash
if [[ "$1" == "-v" ]]; then
    echo "PHP 8.1.0 (cli) (built: Jan  1 2023 10:30:45)"
elif [[ "$1" == "-m" ]]; then
    echo "[PHP Modules]"
    echo "Core"
    echo "json"
fi
`

	err := os.WriteFile(mockPhpPath, []byte(mockScript), 0755)
	if err != nil {
		t.Fatalf("Failed to create mock PHP binary: %v", err)
	}

	// Clear cache
	resetPHPInfoCache()

	cfg := Target{
		Binary: mockPhpPath,
	}

	ctx := context.Background()

	// First call
	info1, err := GetPHPStats(ctx, cfg)
	if err != nil {
		t.Fatalf("First GetPHPStats failed: %v", err)
	}

	// Second call (should use cache)
	info2, err := GetPHPStats(ctx, cfg)
	if err != nil {
		t.Fatalf("Second GetPHPStats failed: %v", err)
	}

	// The same ANSWER, and deliberately not the same instance.
	//
	// It used to be the same pointer, and Info carries a slice — so one caller
	// sorting or appending to Extensions changed what every other caller saw,
	// from a package that reads as read-only. Two scrapes of the same binary
	// are two callers.
	if info1.Version != info2.Version || len(info1.Extensions) != len(info2.Extensions) {
		t.Errorf("the cached answer changed between calls: %+v then %+v", info1, info2)
	}
	if info1 == info2 {
		t.Error("both callers hold the same *Info; either can change what the other sees")
	}
	if len(info1.Extensions) > 0 {
		info1.Extensions[0] = "mutated-by-one-caller"
		if info2.Extensions[0] == "mutated-by-one-caller" {
			t.Error("the extension list is shared: one caller's edit reached another's copy")
		}
	}

	// The entry is cached per binary.
	phpInfoMu.Lock()
	entry, cached := phpInfoCache[cfg.Binary]
	phpInfoMu.Unlock()

	if !cached || entry.expiresAt.IsZero() {
		t.Errorf("Expected an entry cached for %s", cfg.Binary)
	}

	// Expire it.
	phpInfoMu.Lock()
	entry.expiresAt = time.Now().Add(-time.Minute)
	phpInfoCache[cfg.Binary] = entry
	phpInfoMu.Unlock()

	// Third call (should refresh cache)
	info3, err := GetPHPStats(ctx, cfg)
	if err != nil {
		t.Fatalf("Third GetPHPStats failed: %v", err)
	}

	// Should be a new instance
	if info1 == info3 {
		t.Errorf("Expected refreshed cache to be a different instance")
	}

	// But content should be the same
	if info1.Version != info3.Version {
		t.Errorf("Expected version to be the same after cache refresh")
	}
}

func TestGetPHPVersion(t *testing.T) {
	tests := []struct {
		name           string
		phpOutput      string
		expectedResult string
		expectError    bool
	}{
		{
			name: "standard PHP version",
			phpOutput: `PHP 8.2.10 (cli) (built: Sep  1 2023 10:30:45)
Copyright (c) The PHP Group
Zend Engine v4.2.10, Copyright (c) Zend Technologies`,
			expectedResult: "PHP 8.2.10 (cli) (built: Sep  1 2023 10:30:45)",
			expectError:    false,
		},
		{
			name: "PHP 7.4 version",
			phpOutput: `PHP 7.4.33 (cli) (built: May 16 2023 10:30:45)
Copyright (c) The PHP Group
Zend Engine v3.4.0, Copyright (c) Zend Technologies`,
			expectedResult: "PHP 7.4.33 (cli) (built: May 16 2023 10:30:45)",
			expectError:    false,
		},
		{
			name:           "empty output",
			phpOutput:      "",
			expectedResult: "", // Empty output results in empty string, not "unknown"
			expectError:    false,
		},
		{
			name:           "single line",
			phpOutput:      "PHP 8.0.0 (cli)",
			expectedResult: "PHP 8.0.0 (cli)",
			expectError:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create mock binary
			tempDir := t.TempDir()
			mockPhpPath := tempDir + "/mock-php-version"

			mockScript := `#!/bin/bash
cat << 'EOF'
` + tt.phpOutput + `
EOF`

			err := os.WriteFile(mockPhpPath, []byte(mockScript), 0755)
			if err != nil {
				t.Fatalf("Failed to create mock PHP binary: %v", err)
			}

			result, err := getPHPVersion(context.Background(), mockPhpPath)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got none")
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				} else if result != tt.expectedResult {
					t.Errorf("Expected '%s', got '%s'", tt.expectedResult, result)
				}
			}
		})
	}
}

func TestGetPHPExtensions(t *testing.T) {
	tests := []struct {
		name           string
		phpOutput      string
		expectedResult []string
		expectError    bool
	}{
		{
			name: "standard extensions output",
			phpOutput: `[PHP Modules]
Core
date
filter
hash
json
pcre
Reflection
SPL

[Zend Modules]
Zend OPcache`,
			expectedResult: []string{"Core", "date", "filter", "hash", "json", "pcre", "Reflection", "SPL", "Zend OPcache"},
			expectError:    false,
		},
		{
			name: "minimal extensions",
			phpOutput: `[PHP Modules]
Core
json

[Zend Modules]`,
			expectedResult: []string{"Core", "json"},
			expectError:    false,
		},
		{
			name:           "empty output",
			phpOutput:      "",
			expectedResult: []string{},
			expectError:    false,
		},
		{
			name: "only sections",
			phpOutput: `[PHP Modules]

[Zend Modules]`,
			expectedResult: []string{},
			expectError:    false,
		},
		{
			name: "mixed content",
			phpOutput: `[PHP Modules]
Core
filter
[Some Other Section]
Other content
hash
json`,
			expectedResult: []string{"Core", "filter", "Other content", "hash", "json"},
			expectError:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create mock binary
			tempDir := t.TempDir()
			mockPhpPath := tempDir + "/mock-php-extensions"

			mockScript := `#!/bin/bash
cat << 'EOF'
` + tt.phpOutput + `
EOF`

			err := os.WriteFile(mockPhpPath, []byte(mockScript), 0755)
			if err != nil {
				t.Fatalf("Failed to create mock PHP binary: %v", err)
			}

			result, err := getPHPExtensions(context.Background(), mockPhpPath)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got none")
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				} else {
					if len(result) != len(tt.expectedResult) {
						t.Errorf("Expected %d extensions, got %d", len(tt.expectedResult), len(result))
					} else {
						for i, expected := range tt.expectedResult {
							if result[i] != expected {
								t.Errorf("Expected extension[%d] to be '%s', got '%s'", i, expected, result[i])
							}
						}
					}
				}
			}
		})
	}
}

func TestGetPHPStats_ErrorHandling(t *testing.T) {

	// Clear cache
	resetPHPInfoCache()

	// Test with non-existent binary
	cfg := Target{
		Binary: "/non/existent/php",
	}

	ctx := context.Background()
	_, err := GetPHPStats(ctx, cfg)
	if err == nil {
		t.Errorf("Expected error for non-existent binary")
	}

	// The failure is cached, with its own timestamp: without one, a binary
	// that errors re-forked php -v for every pool on every scrape forever.
	phpInfoMu.Lock()
	entry, cached := phpInfoCache[cfg.Binary]
	phpInfoMu.Unlock()

	if !cached || entry.err == nil {
		t.Errorf("Expected the failure to be cached")
	}
	if entry.expiresAt.IsZero() {
		t.Errorf("Expected the failure entry to expire, not be retried every scrape")
	}

	// Second call should return cached error
	_, err2 := GetPHPStats(ctx, cfg)
	if err2 == nil {
		t.Errorf("Expected cached error on second call")
	}

}

func TestPHPStats_ConcurrentAccess(t *testing.T) {

	// Create mock PHP binary
	tempDir := t.TempDir()
	mockPhpPath := tempDir + "/mock-php-concurrent"

	mockScript := `#!/bin/bash
if [[ "$1" == "-v" ]]; then
    echo "PHP 8.0.0 (cli)"
elif [[ "$1" == "-m" ]]; then
    echo "[PHP Modules]"
    echo "Core"
    echo "json"
fi
sleep 0.1
`

	err := os.WriteFile(mockPhpPath, []byte(mockScript), 0755)
	if err != nil {
		t.Fatalf("Failed to create mock PHP binary: %v", err)
	}

	// Clear cache
	resetPHPInfoCache()

	cfg := Target{
		Binary: mockPhpPath,
	}

	ctx := context.Background()

	// Launch multiple goroutines to test concurrent access
	results := make(chan *Info, 5)
	errors := make(chan error, 5)

	for i := 0; i < 5; i++ {
		go func() {
			info, err := GetPHPStats(ctx, cfg)
			if err != nil {
				errors <- err
				return
			}
			results <- info
		}()
	}

	// Collect results
	var infos []*Info
	for i := 0; i < 5; i++ {
		select {
		case info := <-results:
			infos = append(infos, info)
		case err := <-errors:
			t.Fatalf("Concurrent GetPHPStats failed: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatalf("Timeout waiting for concurrent GetPHPStats")
		}
	}

	// All the same answer, each its own copy. Coalescing onto one fork is the
	// point; sharing the result out of it is the bug.
	for i := 1; i < len(infos); i++ {
		if infos[i].Version != infos[0].Version {
			t.Errorf("concurrent callers got different answers: %q and %q",
				infos[i].Version, infos[0].Version)
		}
		if infos[i] == infos[0] {
			t.Error("two concurrent callers hold the same *Info")
		}
	}
}

// A host running php8.1-fpm and php8.3-fpm side by side is exactly what
// side by side. A single unkeyed cache meant
// whichever pool was scraped first decided the version every other pool
// reported for the next hour.
func TestGetPHPStats_CacheIsKeyedByBinary(t *testing.T) {
	resetPHPInfoCache()
	t.Cleanup(resetPHPInfoCache)

	dir := t.TempDir()
	write := func(name, version string) string {
		path := filepath.Join(dir, name)
		script := "#!/bin/sh\ncase \"$1\" in\n-v) echo \"PHP " + version + " (cli)\" ;;\n-m) echo core ;;\nesac\n"
		if err := os.WriteFile(path, []byte(script), 0755); err != nil {
			t.Fatalf("Failed to write fake php: %v", err)
		}
		return path
	}

	old := write("php81", "8.1.30")
	recent := write("php83", "8.3.14")

	ctx := context.Background()

	first, err := GetPHPStats(ctx, Target{Binary: old})
	if err != nil {
		t.Fatalf("GetPHPStats(old) failed: %v", err)
	}
	second, err := GetPHPStats(ctx, Target{Binary: recent})
	if err != nil {
		t.Fatalf("GetPHPStats(recent) failed: %v", err)
	}

	if !strings.Contains(first.Version, "8.1.30") {
		t.Errorf("Expected the 8.1 binary's version, got %q", first.Version)
	}
	if !strings.Contains(second.Version, "8.3.14") {
		t.Errorf("Expected the 8.3 binary's version, got %q", second.Version)
	}
}
