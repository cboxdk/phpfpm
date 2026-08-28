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
	return reload(pid, "")
}

// ReloadMaster is Reload for a caller that knows which master it means.
//
// VerifyMaster on its own only establishes that the pid is *a* php-fpm master.
// That is enough on a host running one, and not enough on a host running
// several: a master can exit between discovery and the reload, and a pid handed
// straight back to a different master would be signalled as though it were ours.
// The config path is in the process title, so checking it costs nothing and
// makes the answer specific.
func ReloadMaster(pid int, configPath string) error {
	return reload(pid, configPath)
}

func reload(pid int, configPath string) error {
	if err := verifyMaster(pid, configPath); err != nil {
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

// ReloadTarget identifies a master well enough to recognise it after a reload.
//
// The pid alone is not enough, because a reload does not always preserve it.
type ReloadTarget struct {
	// PID is the master to signal.
	PID int

	// PIDFile and ConfigPath are how the master is found again if it comes back
	// under a different pid. Either is sufficient; both are better.
	PIDFile    string
	ConfigPath string
}

// ReloadAndWait reloads a master and waits for it to come back, returning the
// pid it came back as.
//
// A reload that kills the master is the failure this exists to detect: Reload
// returns as soon as the signal is delivered, which is well before the master
// has re-read anything.
//
// The returned pid may differ from the one signalled, and that is the whole
// subtlety. SIGUSR2 makes the master re-exec itself. When php-fpm runs in the
// foreground — under systemd, or as pid 1 in a container — the pid survives.
// When it runs DAEMONIZED, which is php-fpm's own default, the re-exec produces
// a new process and the original exits.
//
// Watching the original pid therefore reported a perfectly successful reload as
// a dead master. Observed on a stock homebrew php-fpm: the log said "using
// inherited socket" and "ready to handle connections" under a new pid while the
// caller rolled the change back and told the operator its master had died.
//
// So the confirmation is about the MASTER, not the number: the original pid
// surviving is one way to see it, and a successor that owns the same config is
// the other.
func ReloadAndWait(ctx context.Context, target ReloadTarget, settle time.Duration, log *slog.Logger) (int, error) {
	log = logOrDiscard(log)

	if err := ReloadMaster(target.PID, target.ConfigPath); err != nil {
		return 0, err
	}

	log.Debug("Reloaded php-fpm master", "pid", target.PID, "settle", settle)

	deadline := time.Now().Add(settle)
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}

		switch {
		case processAlive(target.PID):
			// Still the same process. Keep watching until the settle window is
			// over: a master that fails to re-read its configuration dies a
			// moment after the signal, not instantly.
			if time.Now().After(deadline) {
				return target.PID, nil
			}

		default:
			// The pid is gone. Either the master died, or it re-execed into a
			// new one — indistinguishable from here, so go and look.
			if pid, ok := successor(target, log); ok {
				log.Info("The master came back under a new pid, as a daemonized reload does",
					"was", target.PID, "now", pid)

				return pid, nil
			}
			if time.Now().After(deadline) {
				return 0, fmt.Errorf("php-fpm master %d exited during reload and no master took its place",
					target.PID)
			}
		}

		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// successor looks for the master that replaced one that has gone.
func successor(target ReloadTarget, log *slog.Logger) (int, bool) {
	if target.PIDFile != "" {
		if pid, err := MasterPID(target.PIDFile); err == nil && pid != target.PID {
			return pid, true
		}
	}

	if target.ConfigPath == "" {
		return 0, false
	}

	// No pid file, or one not yet rewritten: scan for a master serving the same
	// configuration. Matching on the config is what makes this safe on a host
	// running several masters — a different one being alive says nothing about
	// whether ours came back.
	//
	// DiscoverMasters rather than Discover: this runs in the middle of a reload,
	// and Discover would execute `php-fpm -tt` against every master on the host
	// to parse pools nobody is asking about — against a configuration that is
	// being re-read at that exact moment.
	found, err := DiscoverMasters(log)
	if err != nil {
		return 0, false
	}
	for _, m := range found {
		if m.ConfigPath == target.ConfigPath && m.PID > 0 && m.PID != target.PID {
			return m.PID, true
		}
	}

	return 0, false
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
	return verifyMaster(pid, "")
}

// verifyMaster checks that pid is a php-fpm master, and — when configPath is
// given — that it is the master serving THAT configuration.
//
// There is a residual window between this check and the signal that follows it:
// the process could exit and the pid be reused in between. Closing it entirely
// would need a pidfd, which does not exist on every platform this builds for.
// It is narrow on purpose instead: the replacement would have to be a php-fpm
// master, started in the microseconds between two adjacent syscalls, serving the
// same configuration file. Before this existed the check was `pid <= 1`, which
// admitted any process at all.
func verifyMaster(pid int, configPath string) error {
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

	if configPath != "" && !strings.Contains(cmdline, configPath) {
		return fmt.Errorf("%w: pid %d is a php-fpm master, but for %q rather than %q",
			ErrNotAMaster, pid, cmdline, configPath)
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
