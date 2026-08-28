package phpfpm

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"regexp"
	"strconv"
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

	// MaxChildren and ProcessManager are the pool's CONFIGURED settings, read
	// from the effective configuration during discovery.
	//
	// Carried because a caller that cannot reach a pool still needs them. A pool
	// whose socket refuses — restarting, or briefly overloaded — is not a pool
	// that has stopped occupying memory, and a consumer with no idea how large
	// it is will hand its allocation to a neighbour and overcommit the host the
	// moment it comes back. Discovery has already parsed this; throwing it away
	// only to be unable to recover it later is the expensive kind of tidy.
	MaxChildren    int
	ProcessManager string

	// PID is the master process serving this pool.
	//
	// Carried from the process scan because the pid file is not a reliable
	// alternative: the official php:8.3-fpm image ships `pid` commented out, so
	// there is no file to read — and a caller that could not identify the master
	// would write pool configuration and never reload it.
	PID int
}

var fpmNamePattern = regexp.MustCompile(`^php[0-9]{0,2}.*fpm.*$`)

// Master is a running php-fpm master, identified WITHOUT reading its
// configuration.
//
// Discover parses each master's effective config, which is what makes it
// useful — and what makes it useless in the one situation a caller most needs
// an answer. A master whose config file no longer parses is skipped entirely,
// so a tool trying to repair exactly that config cannot find the master to
// repair it for. Observed: a rejected pool fragment left on disk by a run that
// died, a healthy master still serving from the configuration it loaded before
// the file appeared, and `fpm-tune apply` reporting "no PHP-FPM pools found"
// while the fragment sat there waiting for any reload to adopt it.
//
// This carries only what the process table itself provides, so it answers even
// when the configuration does not.
type Master struct {
	PID        int
	Binary     string
	ConfigPath string
}

// DiscoverMasters scans the process table for php-fpm masters.
//
// The binary and config path still go through the trust checks — they are about
// to be handed to exec — but nothing is executed here and nothing is parsed.
//
// log may be nil.
func DiscoverMasters(log *slog.Logger) ([]Master, error) {
	log = logOrDiscard(log)

	procs, err := process.Processes()
	if err != nil {
		return nil, fmt.Errorf("failed to list processes: %w", err)
	}

	var masters []Master
	for _, p := range procs {
		exe, config, ok := masterIdentity(p, log)
		if !ok {
			continue
		}
		masters = append(masters, Master{PID: int(p.Pid), Binary: exe, ConfigPath: config})
	}

	return masters, nil
}

// masterIdentity is the part of discovery that reads nothing but the process
// table, shared so the two entry points cannot drift apart on what counts as a
// master or on which paths are trusted.
func masterIdentity(p *process.Process, log *slog.Logger) (binary, config string, ok bool) {
	name, err := p.Name()
	if err != nil || !fpmNamePattern.MatchString(filepath.Base(name)) {
		return "", "", false
	}

	cmdlineStr, err := p.Cmdline()
	if err != nil || !strings.Contains(cmdlineStr, "master process") {
		return "", "", false
	}

	config = extractConfigFromMaster(cmdlineStr)
	if config == "" {
		return "", "", false
	}

	exe, err := p.Exe()
	if err != nil {
		log.Debug("Cannot determine binary path", "pid", p.Pid, "error", err)

		return "", "", false
	}

	// The process table is not a trust boundary: any local user can start a
	// process whose name matches and whose command line names a config path
	// they control. Both are about to be handed to exec.
	if err := trustedPath(exe); err != nil {
		logSkip(log, err, "Refusing to run discovered PHP-FPM binary",
			"pid", p.Pid, "binary", exe, "reason", err)

		return "", "", false
	}
	if err := trustedPath(config); err != nil {
		logSkip(log, err, "Refusing to read discovered PHP-FPM config",
			"pid", p.Pid, "config", config, "reason", err)

		return "", "", false
	}

	return exe, config, true
}

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
		exe, config, ok := masterIdentity(p, log)
		if !ok {
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

			maxChildren, _ := strconv.Atoi(strings.TrimSpace(poolConfig["pm.max_children"]))
			processManager := strings.TrimSpace(poolConfig["pm"])

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

				MaxChildren:    maxChildren,
				ProcessManager: processManager,
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

// logSkip reports a process discovery passed over, at a level that matches what
// it means.
//
// A path that has been REMOVED is routine: a master left behind by a container
// that is gone, or by a directory since deleted. Any machine that has run
// php-fpm more than once accumulates them, and warning about each one meant
// every command opened with a screen of noise about processes the operator has
// not asked anyone to manage — seven of them on the development machine this
// was found on, before a single useful line.
//
// A path that EXISTS and fails the checks is different, and stays a warning:
// something is running php-fpm from a binary or config this process is not
// willing to trust, and an operator wanting that pool managed needs to know why
// it was skipped.
func logSkip(log *slog.Logger, err error, msg string, args ...any) {
	if errors.Is(err, ErrPathMissing) {
		log.Debug(msg+" (the path no longer exists; the process is a leftover)", args...)

		return
	}

	log.Warn(msg, args...)
}
