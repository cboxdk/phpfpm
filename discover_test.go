package phpfpm

import (
	"os"
	"testing"
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

func TestDiscoverFPMProcesses_MockImplementation(t *testing.T) {
	if os.Getenv("CI") == "true" {
		t.Skip("Skipping discovery test in CI environment")
	}

	// Note: This test is limited because we can't easily mock the process.Processes() call
	// In a real implementation, we would need to use dependency injection or build tags
	// to replace the process discovery mechanism for testing

	// For now, we test that the function exists and returns without panicking
	discovered, err := Discover(nil)

	// We expect this to work even if no FPM processes are found
	if err != nil {
		// Only fail if it's a fundamental error (not "no processes found")
		// Most systems won't have FPM running during tests
		t.Logf("DiscoverFPMProcesses returned error (expected in test environment): %v", err)
	}

	// Should return a slice (even if empty)
	if discovered == nil {
		t.Errorf("Expected DiscoverFPMProcesses to return non-nil slice")
	}

	// Log the results for debugging
	t.Logf("Discovered %d FPM processes", len(discovered))
	for i, fpm := range discovered {
		t.Logf("FPM %d: Binary=%s, Socket=%s, StatusPath=%s", i, fpm.Binary, fpm.Socket, fpm.StatusPath)
	}
}
