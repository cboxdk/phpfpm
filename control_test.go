package phpfpm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestValidateRejectsABadConfig is the guard that makes writing pool
// configuration survivable: PHP-FPM does not fail gracefully on a bad reload,
// the master refuses to come back and takes every pool with it. If Validate ever
// stops reporting a broken config, that outage becomes reachable again.
func TestValidateRejectsABadConfig(t *testing.T) {
	binary := lookupFPM(t)

	dir := t.TempDir()
	bad := filepath.Join(dir, "broken.conf")
	if err := os.WriteFile(bad, []byte("[global\nthis is not a config\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := Validate(context.Background(), binary, bad)
	if err == nil {
		t.Fatal("a malformed config validated; a reload with it would take the pools down")
	}
	if !strings.Contains(err.Error(), "rejected the configuration") {
		t.Errorf("error does not say what happened: %v", err)
	}
}

// TestValidateWithoutABinary keeps the failure legible rather than exec'ing "".
func TestValidateWithoutABinary(t *testing.T) {
	if err := Validate(context.Background(), "", "/etc/php-fpm.conf"); err == nil {
		t.Error("an empty binary path was accepted")
	}
}

// TestReloadRefusesInit: pid 1 is the container's init, not a discovered
// php-fpm master. Sending it SIGUSR2 is a different and much worse event than
// the one the caller asked for.
func TestReloadRefusesInit(t *testing.T) {
	for _, pid := range []int{0, 1, -1} {
		if err := Reload(pid); err == nil {
			t.Errorf("Reload(%d) was accepted", pid)
		}
	}
}

// TestReloadDeliversSIGUSR2 checks the signal actually arrives, using a process
// that reports having received it.
func TestReloadDeliversSIGUSR2(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "reloaded")

	// The shell reports readiness by touching a file AFTER installing the trap.
	// Waiting on the pid instead would be a race that hides the bug: the process
	// exists the instant it starts, and the default action for USR2 is to
	// terminate — so signalling too early kills it and looks like a delivery
	// failure (or, in the death test below, like a pass).
	cmd := startSignalTarget(t, "trap 'echo yes > "+marker+"' USR2", dir)

	if err := Reload(cmd.Process.Pid); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	if !waitFor(t, 3*time.Second, func() bool {
		_, err := os.Stat(marker)
		return err == nil
	}) {
		t.Error("the process never saw SIGUSR2")
	}
}

// TestReloadAndWaitReportsAMasterThatDies is the case the plain Reload cannot
// see: the signal is delivered successfully and the master then fails to come
// back. A caller that treats delivery as success would leave the pools down and
// never roll back.
func TestReloadAndWaitReportsAMasterThatDies(t *testing.T) {
	// Exits on USR2 instead of reloading, standing in for a master that cannot
	// re-read its configuration.
	cmd := startSignalTarget(t, "trap 'exit 1' USR2", t.TempDir())

	// Reap the child as it exits, so the pid stops resolving rather than
	// lingering as a zombie that still answers signal 0.
	done := make(chan struct{})
	go func() { _, _ = cmd.Process.Wait(); close(done) }()

	err := ReloadAndWait(context.Background(), cmd.Process.Pid, 2*time.Second, nil)
	<-done

	if err == nil {
		t.Error("a master that exited during reload was reported as a success")
	}
}

// TestReloadAndWaitAcceptsAMasterThatSurvives is the happy path.
func TestReloadAndWaitAcceptsAMasterThatSurvives(t *testing.T) {
	cmd := startSignalTarget(t, "trap ':' USR2", t.TempDir())

	if err := ReloadAndWait(context.Background(), cmd.Process.Pid, 300*time.Millisecond, nil); err != nil {
		t.Errorf("a surviving master was reported as a failure: %v", err)
	}
}

func TestMasterPID(t *testing.T) {
	dir := t.TempDir()

	t.Run("reads a live pid", func(t *testing.T) {
		f := filepath.Join(dir, "live.pid")
		if err := os.WriteFile(f, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		pid, err := MasterPID(f)
		if err != nil {
			t.Fatalf("MasterPID: %v", err)
		}
		if pid != os.Getpid() {
			t.Errorf("pid = %d, want %d", pid, os.Getpid())
		}
	})

	t.Run("rejects a stale pid file", func(t *testing.T) {
		// A pid file left behind by a crashed master is the common case, and
		// signalling whatever now owns that pid is the reason it matters.
		f := filepath.Join(dir, "stale.pid")
		if err := os.WriteFile(f, []byte("999999\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := MasterPID(f); err == nil {
			t.Error("a pid file naming a dead process was accepted")
		}
	})

	t.Run("rejects implausible contents", func(t *testing.T) {
		for name, body := range map[string]string{
			"not a number": "php-fpm\n",
			"init":         "1\n",
			"empty":        "",
		} {
			f := filepath.Join(dir, "bad-"+strings.ReplaceAll(name, " ", "-")+".pid")
			if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := MasterPID(f); err == nil {
				t.Errorf("%s was accepted", name)
			}
		}
	})

	t.Run("reports a missing file", func(t *testing.T) {
		if _, err := MasterPID(filepath.Join(dir, "absent.pid")); err == nil {
			t.Error("a missing pid file was accepted")
		}
	})
}

func TestProcessAlive(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Error("the test process reports as not running")
	}
	if processAlive(999999) {
		t.Error("an unused pid reports as running")
	}
}

// startSignalTarget launches a shell with the given trap installed and does not
// return until the trap is actually in place.
//
// The readiness handshake is the point: every one of these tests signals the
// process, and the default disposition for SIGUSR2 is termination. Racing the
// trap makes a delivery test fail and a death test pass, for the same reason and
// with neither telling you so.
func startSignalTarget(t *testing.T, trap, dir string) *exec.Cmd {
	t.Helper()

	ready := filepath.Join(dir, "ready")
	cmd := exec.Command("/bin/sh", "-c", trap+"; touch "+ready+"; sleep 10 & wait")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	if !waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(ready)
		return err == nil
	}) {
		t.Fatal("the shell never signalled that its trap was installed")
	}

	return cmd
}

// lookupFPM finds a php-fpm binary or skips. Validate shells out, so there is
// nothing to assert without one.
func lookupFPM(t *testing.T) string {
	t.Helper()

	for _, candidate := range []string{"php-fpm", "php-fpm8.4", "php-fpm8.3", "php-fpm8.2", "php-fpm8.1"} {
		if path, err := exec.LookPath(candidate); err == nil {
			return path
		}
	}
	t.Skip("no php-fpm binary available")

	return ""
}

// waitFor polls until cond holds or the budget runs out.
func waitFor(t *testing.T, budget time.Duration, cond func() bool) bool {
	t.Helper()

	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}

	return cond()
}
