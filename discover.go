package phpfpm

import (
	"bytes"
	"fmt"
	"log/slog"
	"net"
	"os/exec"
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
	CliBinary    string
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

			cliBinary, _ := findMatchingCliBinary(exe)

			found = append(found, Discovered{
				Name:         poolName,
				ConfigPath:   config,
				StatusPath:   status,
				Binary:       exe,
				Socket:       socket,
				StatusSocket: statusSocket,
				CliBinary:    cliBinary,
			})

			log.Debug("Discovered php-fpm pool",
				"config", config,
				"pool", poolName,
				"socket", socket,
				"status_socket", statusSocket,
				"status_path", status,
				"cli_binary", cliBinary,
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

// findMatchingCliBinary attempts to find the php-cli binary that matches the version of the FPM binary.
func findMatchingCliBinary(fpmBinary string) (string, error) {
	out, err := exec.Command(fpmBinary, "-v").Output()
	if err != nil {
		return "", fmt.Errorf("failed to get version from fpm binary: %w", err)
	}
	re := regexp.MustCompile(`PHP (\d+\.\d+)`)
	matches := re.FindSubmatch(out)
	if len(matches) < 2 {
		return "", fmt.Errorf("could not parse PHP version from output: %s", string(out))
	}
	version := string(matches[1]) // e.g. "8.2"

	candidates := []string{
		filepath.Join("/usr/bin", "php"+version),
		filepath.Join("/usr/local/bin", "php"+version),
		"php" + version, // Fallback til PATH
		"php",           // Sidste fallback
	}

	for _, cli := range candidates {
		out, err := exec.Command(cli, "-v").Output()
		if err != nil {
			continue
		}
		if bytes.Contains(out, []byte(version)) && bytes.Contains(out, []byte("cli")) {
			return cli, nil
		}
	}
	return "", fmt.Errorf("matching php-cli binary for version %s not found", version)
}
