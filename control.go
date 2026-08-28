package phpfpm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v3/process"
)

// Validate checks a configuration with `php-fpm -t` without applying it.
//
// This is the guard that makes writing pool configuration survivable. PHP-FPM
// re-reads its configuration on reload, and a syntax error or an impossible
// value does not fail gracefully — the master refuses to come back and every
// pool it served goes down. Validating first turns that outage into a returned
// error.
//
// The configPath is the master config, not the drop-in: `-t` walks the include
// tree, so a broken fragment is caught through its parent.
func Validate(ctx context.Context, binary, configPath string) error {
	if binary == "" {
		return errors.New("no php-fpm binary given")
	}

	args := []string{"-t"}
	if configPath != "" {
		args = append(args, "--fpm-config", configPath)
	}

	cmd := exec.CommandContext(ctx, binary, args...)

	// php-fpm writes its test report to stderr, including on success.
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("php-fpm rejected the configuration: %w\n%s", err, strings.TrimSpace(string(output)))
	}

	return nil
}

// Reload asks a running master to re-read its configuration.
//
// SIGUSR2 is a graceful reload: the master re-reads the config and cycles its
// workers, letting each finish the request it is serving. It is deliberately not
// a restart — a restart drops in-flight requests, and on a host serving many
// sites that is a visible outage for a change that was meant to be routine.
//
// Callers changing pool settings should Validate first. Reload does not check
// the configuration it is asking the master to adopt.
func Reload(pid int) error {
	if err := VerifyMaster(pid); err != nil {
		return err
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("no process %d: %w", pid, err)
	}

	if err := proc.Signal(syscall.SIGUSR2); err != nil {
		return fmt.Errorf("failed to signal php-fpm master %d: %w", pid, err)
	}

	return nil
}

// ReloadAndWait reloads a master and waits for it to come back.
//
// A reload that kills the master is the failure this exists to detect: Reload
// itself returns nil as soon as the signal is delivered, which is well before
// the master has re-read anything. Callers that are about to rely on the new
// configuration — or that need to know whether to roll back — need the
// confirmation rather than the delivery.
//
// Liveness is checked with signal 0, which tests for the process without
// delivering anything.
func ReloadAndWait(ctx context.Context, pid int, settle time.Duration, log *slog.Logger) error {
	log = logOrDiscard(log)

	if err := Reload(pid); err != nil {
		return err
	}

	log.Debug("Reloaded php-fpm master", "pid", pid, "settle", settle)

	deadline := time.Now().Add(settle)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !processAlive(pid) {
			return fmt.Errorf("php-fpm master %d exited during reload", pid)
		}
		if time.Now().After(deadline) {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// ErrNotAMaster reports that a pid is not a php-fpm master process.
var ErrNotAMaster = errors.New("not a php-fpm master process")

// VerifyMaster confirms that a pid really is a php-fpm master, immediately
// before it is signalled.
//
// This replaced a `pid <= 1` refusal that was wrong in both directions.
//
// It refused the most common deployment there is. In the official php:8.3-fpm
// image the master IS pid 1, so `fpm-tune apply` wrote the configuration,
// declined to reload, reported the master as dead and rolled the whole change
// back — verified against that image, where every apply failed this way.
//
// And it did not guard against the thing that actually matters. A pid is only
// a promise about the instant it was read: between discovery and the reload the
// master can exit and the kernel can hand the number to something else. SIGUSR2
// to a process that did not ask for it terminates it by default, so the old
// check would happily kill an unrelated program while carefully declining to
// signal init. Container pid namespaces start at 1 and stay small, which is
// exactly where recycling is likely.
//
// Checking what the process IS covers both, and costs one /proc read.
func VerifyMaster(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("%w: pid %d", ErrNotAMaster, pid)
	}

	proc, err := process.NewProcess(int32(pid))
	if err != nil {
		return fmt.Errorf("%w: no process %d: %w", ErrNotAMaster, pid, err)
	}

	cmdline, err := proc.Cmdline()
	if err != nil {
		return fmt.Errorf("%w: cannot read the command line of pid %d: %w", ErrNotAMaster, pid, err)
	}

	// The same signature Discover matches on: php-fpm sets its process title to
	// "php-fpm: master process (/path/to/php-fpm.conf)".
	if !strings.Contains(cmdline, "master process") || !strings.Contains(cmdline, "php-fpm") {
		return fmt.Errorf("%w: pid %d is %q", ErrNotAMaster, pid, cmdline)
	}

	return nil
}

// processAlive reports whether a pid is still running, without disturbing it.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}

	return proc.Signal(syscall.Signal(0)) == nil
}

// MasterPID reads a php-fpm master's pid from its pid file.
//
// Discover finds masters by scanning the process table, which is the right tool
// when you do not know what is running. When you already know which pool you are
// reloading, the pid file is the authoritative answer and does not require
// permission to read another user's /proc entry.
func MasterPID(pidFile string) (int, error) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return 0, fmt.Errorf("cannot read pid file %s: %w", pidFile, err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("pid file %s does not contain a pid: %w", pidFile, err)
	}
	if err := VerifyMaster(pid); err != nil {
		// A stale pid file is ordinary — the master was killed, the file stayed —
		// and the number in it may since have been reused by something else.
		return 0, fmt.Errorf("pid file %s names pid %d: %w", pidFile, pid, err)
	}

	return pid, nil
}
