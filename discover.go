package phpfpm

import (
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v3/process"
)

// Discovered is one pool found by scanning the process table.
type Discovered struct {
	Name         string
	ConfigPath   string
	StatusPath   string
	Binary       string
	Socket       string
	StatusSocket string

	// PID is the master process serving this pool.
	//
	// Carried from the process scan because the pid file is not a reliable
	// alternative: the official php:8.3-fpm image ships `pid` commented out, so
	// there is no file to read — and a caller that could not identify the master
	// would write pool configuration and never reload it.
	PID int
}

var fpmNamePattern = regexp.MustCompile(`^php[0-9]{0,2}.*fpm.*$`)

// Discover scans the process table for PHP-FPM masters and returns the pools
// they serve.
//
// It parses each master's effective configuration, which means executing the
// discovered binary — see trustedPath for why that is gated on ownership.
//
// log may be nil.
func Discover(log *slog.Logger) ([]Discovered, error) {
	log = logOrDiscard(log)

	procs, err := process.Processes()
	if err != nil {
		return []Discovered{}, fmt.Errorf("failed to list processes: %w", err)
	}

	found := make([]Discovered, 0)

	for _, p := range procs {
		name, err := p.Name()
		if err != nil || !fpmNamePattern.MatchString(filepath.Base(name)) {
			continue
		}

		cmdlineStr, err := p.Cmdline()
		if err != nil || !strings.Contains(cmdlineStr, "master process") {
			continue
		}

		config := extractConfigFromMaster(cmdlineStr)
		if config == "" {
			continue
		}

		exe, err := p.Exe()
		if err != nil {
			log.Debug("Cannot determine binary path", "pid", p.Pid, "error", err)
			continue
		}

		// The process table is not a trust boundary: any local user can start a
		// process whose name matches and whose command line names a config path
		// they control. Both are about to be handed to exec.
		if err := trustedPath(exe); err != nil {
			log.Warn("Refusing to run discovered PHP-FPM binary",
				"pid", p.Pid, "binary", exe, "reason", err)
			continue
		}
		if err := trustedPath(config); err != nil {
			log.Warn("Refusing to read discovered PHP-FPM config",
				"pid", p.Pid, "config", config, "reason", err)
			continue
		}

		parsed, err := ParseConfig(exe, config)
		if err != nil {
			log.Error("Failed to parse FPM config", "config", config, "error", err)
			continue
		}

		for poolName, poolConfig := range parsed.Pools {
			socket := parseSocket(poolConfig["listen"], log)
			if socket == "" {
				continue
			}

			statusSocket := parseSocket(poolConfig["status_listen"], log)
			if statusSocket == "" {
				statusSocket = socket
			}

			status := poolConfig["pm.status_path"]
			if status == "" {
				status = parsed.Global["pm.status_path"]
			}
			if status == "" {
				log.Debug("Skipping pool with no status path", "pool", poolName, "config", config)
				continue
			}

			found = append(found, Discovered{
				Name:         poolName,
				ConfigPath:   config,
				StatusPath:   status,
				Binary:       exe,
				Socket:       socket,
				StatusSocket: statusSocket,
				PID:          int(p.Pid),
			})

			log.Debug("Discovered php-fpm pool",
				"config", config,
				"pool", poolName,
				"socket", socket,
				"status_socket", statusSocket,
				"status_path", status,
			)
		}
	}

	return found, nil
}

func parseSocket(socket string, log *slog.Logger) string {
	// Normalised here rather than trusting the caller: this is reached from a
	// fallback path that only runs for a bare port number, so a nil logger
	// crashes on exactly the input that is least often exercised.
	log = logOrDiscard(log)

	if socket == "" {
		return ""
	}
	if strings.HasPrefix(socket, "/") {
		return "unix://" + socket
	} else if strings.Contains(socket, ":") {
		return "tcp://" + socket
	} else {
		// fallback if only a port is specified
		try := []string{"127.0.0.1:" + socket, "[::1]:" + socket}
		resolved := ""
		for _, candidate := range try {
			conn, err := net.DialTimeout("tcp", candidate, 500*time.Millisecond)
			if err == nil {
				_ = conn.Close()
				resolved = candidate
				break
			} else {
				log.Warn("Failed to connect to PHP-FPM socket", "socket", candidate, "error", err)
			}
		}
		if resolved != "" {
			return "tcp://" + resolved
		}
	}
	return ""
}

func extractConfigFromMaster(cmdline string) string {
	start := strings.Index(cmdline, "(")
	end := strings.Index(cmdline, ")")
	if start != -1 && end != -1 && end > start {
		return cmdline[start+1 : end]
	}
	return ""
}
