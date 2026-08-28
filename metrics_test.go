package phpfpm

import (
	"context"
	"testing"
)

func TestGetMetrics_ErrorHandling(t *testing.T) {

	ctx := context.Background()

	// Test with empty config
	emptyConfig := []Target{}

	results, err := ScrapeAll(ctx, emptyConfig, nil)
	if err != nil {
		t.Errorf("Expected no error with empty config, got: %v", err)
	}

	if len(results) != 0 {
		t.Errorf("Expected empty results with empty config, got %d", len(results))
	}

	// Test with invalid socket
	invalidConfig := []Target{
		{
			Socket:       "invalid-socket",
			StatusSocket: "invalid://socket/path",
			StatusPath:   "/status",
			Binary:       "/usr/sbin/php-fpm",
		},
	}

	// A pool that cannot be scraped is reported as a failed outcome, not
	// dropped: the collector needs it to emit up=0. When every configured pool
	// fails, that is a scrape failure.
	results, err = ScrapeAll(ctx, invalidConfig, nil)
	if err == nil {
		t.Errorf("Expected an error when every configured pool fails")
	}
	if len(results) != 1 {
		t.Fatalf("Expected one outcome per configured pool, got %d", len(results))
	}
	if results[0].Err == nil {
		t.Errorf("Expected the invalid pool to carry its error")
	}

	// Test with non-existent socket
	nonExistentConfig := []Target{
		{
			Socket:       "non-existent",
			StatusSocket: "unix:///non/existent/socket",
			StatusPath:   "/status",
			Binary:       "/usr/sbin/php-fpm",
		},
	}

	results, err = ScrapeAll(ctx, nonExistentConfig, nil)
	if err == nil {
		t.Errorf("Expected an error when the only configured pool is unreachable")
	}
	if len(results) != 1 || results[0].Err == nil {
		t.Errorf("Expected the unreachable pool to be reported with its error, got %+v", results)
	}
	if results[0].Result != nil {
		t.Errorf("Expected no Result for a failed pool")
	}
}

func TestParseAddress(t *testing.T) {
	tests := []struct {
		name               string
		addr               string
		path               string
		expectedScheme     string
		expectedAddress    string
		expectedScriptPath string
		expectError        bool
	}{
		{
			name:               "unix socket with protocol",
			addr:               "unix:///var/run/php-fpm.sock",
			path:               "/status",
			expectedScheme:     "unix",
			expectedAddress:    "/var/run/php-fpm.sock",
			expectedScriptPath: "/status",
			expectError:        false,
		},
		{
			name:               "unix socket without protocol",
			addr:               "/var/run/php-fpm.sock",
			path:               "/status",
			expectedScheme:     "unix",
			expectedAddress:    "/var/run/php-fpm.sock",
			expectedScriptPath: "/status",
			expectError:        false,
		},
		{
			name:               "tcp socket with protocol",
			addr:               "tcp://127.0.0.1:9000",
			path:               "/status",
			expectedScheme:     "tcp",
			expectedAddress:    "127.0.0.1:9000",
			expectedScriptPath: "/status",
			expectError:        false,
		},
		{
			name:        "unsupported protocol",
			addr:        "http://example.com",
			path:        "/status",
			expectError: true,
		},
		{
			name:        "empty address",
			addr:        "",
			path:        "/status",
			expectError: true,
		},
		{
			name:        "invalid format",
			addr:        "invalid-format",
			path:        "/status",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme, address, scriptPath, err := ParseAddress(tt.addr, tt.path)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got none")
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				} else {
					if scheme != tt.expectedScheme {
						t.Errorf("Expected scheme '%s', got '%s'", tt.expectedScheme, scheme)
					}
					if address != tt.expectedAddress {
						t.Errorf("Expected address '%s', got '%s'", tt.expectedAddress, address)
					}
					if scriptPath != tt.expectedScriptPath {
						t.Errorf("Expected scriptPath '%s', got '%s'", tt.expectedScriptPath, scriptPath)
					}
				}
			}
		})
	}
}

func TestPtr(t *testing.T) {
	// Test the ptr helper function
	intVal := 42
	intPtr := ptr(intVal)

	if intPtr == nil {
		t.Fatalf("Expected ptr to return non-nil pointer")
	}

	if *intPtr != intVal {
		t.Errorf("Expected ptr to return pointer to correct value")
	}

	// Test with different types
	stringVal := "test"
	stringPtr := ptr(stringVal)

	if stringPtr == nil {
		t.Fatalf("Expected ptr to work with string")
	}

	if *stringPtr != stringVal {
		t.Errorf("Expected ptr to return pointer to correct string value")
	}

	float64Val := 3.14
	float64Ptr := ptr(float64Val)

	if float64Ptr == nil {
		t.Fatalf("Expected ptr to work with float64")
	}

	if *float64Ptr != float64Val {
		t.Errorf("Expected ptr to return pointer to correct float64 value")
	}
}

func TestGetMetrics_PoolConfigParsing(t *testing.T) {

	// Test that config parsing logic works (even though actual FPM calls will fail)
	ctx := context.Background()

	cfg := []Target{
		{
			Socket:       "pool1",
			StatusSocket: "unix:///var/run/pool1.sock",
			StatusPath:   "/status",
			Binary:       "/usr/sbin/php-fpm1",
			ConfigPath:   "/etc/php1/fpm.conf",
		},
		{
			Socket:       "pool2",
			StatusSocket: "tcp://127.0.0.1:9001",
			StatusPath:   "/fpm-status",
			Binary:       "/usr/sbin/php-fpm2",
			ConfigPath:   "/etc/php2/fpm.conf",
		},
	}

	// Both pools fail to connect, so this is a failed scrape — but every
	// configured pool must still come back as an outcome, in order, so the
	// collector can report each one down individually.
	results, err := ScrapeAll(ctx, cfg, nil)
	if err == nil {
		t.Errorf("Expected an error when both configured pools are unreachable")
	}

	if len(results) != 2 {
		t.Fatalf("Expected an outcome for each of the 2 configured pools, got %d", len(results))
	}
	for i, outcome := range results {
		if outcome.Err == nil {
			t.Errorf("Expected pool %d to carry an error", i)
		}
		if outcome.Socket == "" {
			t.Errorf("Expected pool %d to be labelled with its socket", i)
		}
	}
}

func TestRecountProcesses(t *testing.T) {
	pool := Pool{Processes: []PoolProcess{
		{PID: 1, State: "Running", RequestURI: "/checkout?token=secret", LastRequestCPU: 10, LastRequestMemory: 100},
		{PID: 2, State: "Idle", RequestURI: "/home", LastRequestCPU: 20, LastRequestMemory: 200},
		{PID: 3, State: "Reading headers", RequestURI: "/api/v1", LastRequestCPU: 30, LastRequestMemory: 300},
		// The exporter's own traffic must not count towards the averages.
		{PID: 4, State: "Running", RequestURI: "/status?json&full", LastRequestCPU: 999, LastRequestMemory: 999},
		{PID: 5, State: "Running", RequestURI: "/" + opcacheScriptPrefix + "123.php", LastRequestCPU: 999, LastRequestMemory: 999},
	}}

	recountProcesses(&pool, "/status")

	if pool.ActiveProcesses != 4 {
		t.Errorf("Expected 4 active (running, reading headers), got %d", pool.ActiveProcesses)
	}
	if pool.IdleProcesses != 1 {
		t.Errorf("Expected 1 idle, got %d", pool.IdleProcesses)
	}
	if pool.TotalProcesses != 5 {
		t.Errorf("Expected 5 total, got %d", pool.TotalProcesses)
	}

	// (10+20+30)/3 — the two self-inflicted requests are excluded.
	if pool.ProcessesCpu == nil || *pool.ProcessesCpu != 20 {
		t.Errorf("Expected the exporter's own requests to be excluded from the CPU average, got %v", pool.ProcessesCpu)
	}
	if pool.ProcessesMemory == nil || *pool.ProcessesMemory != 200 {
		t.Errorf("Expected the exporter's own requests to be excluded from the memory average, got %v", pool.ProcessesMemory)
	}

	if got := pool.Processes[0].RequestURI; got != "/checkout" {
		t.Errorf("Expected the query string to be stripped, got %q", got)
	}
}

// A pool where every process is the exporter's own must not divide by zero.
func TestRecountProcesses_AllExcluded(t *testing.T) {
	pool := Pool{Processes: []PoolProcess{
		{PID: 1, State: "Running", RequestURI: "/status", LastRequestCPU: 5},
	}}

	recountProcesses(&pool, "/status")

	if pool.ProcessesCpu != nil {
		t.Errorf("Expected no CPU average when every request was excluded, got %v", *pool.ProcessesCpu)
	}
}
