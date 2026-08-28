package phpfpm

import (
	"context"
	"errors"
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

// TestReloadRefusesAProcessThatIsNotAMaster.
//
// This replaced a test that asserted pid 1 was refused, which was wrong twice
// over. In the official php:8.3-fpm image the master IS pid 1, so the rule it
// locked in made every apply on the most common deployment there is write the
// configuration, decline to reload, and roll the whole change back — confirmed
// against that image.
//
// And refusing pid 1 never addressed the real hazard. A pid is a promise about
// the instant it was read; between discovery and the reload the master can exit
// and the kernel can hand the number to something else. SIGUSR2 terminates a
// process that has no handler for it, so the old rule would kill an unrelated
// program while carefully declining to signal init.
func TestReloadRefusesAProcessThatIsNotAMaster(t *testing.T) {
	for _, pid := range []int{0, -1} {
		if err := Reload(pid); err == nil {
			t.Errorf("Reload(%d) was accepted", pid)
		}
	}

	// A live process that is not php-fpm: the pid-reuse case, and the one the
	// old check let through.
	cmd := exec.Command("/bin/sh", "-c", "sleep 10")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	err := Reload(cmd.Process.Pid)
	if !errors.Is(err, ErrNotAMaster) {
		t.Errorf("Reload sent SIGUSR2 to a process that is not a php-fpm master (err = %v); "+
			"the default action for USR2 would have killed it", err)
	}
}

// TestReloadAcceptsAMasterThatIsPID1: the container case the old rule refused.
// Verified in the php:8.3-fpm image, where php-fpm is pid 1.
func TestReloadAcceptsAMasterThatIsPID1(t *testing.T) {
	if err := VerifyMaster(1); err == nil {
		// Only true when this test itself runs as pid 1 in a container whose
		// init is php-fpm, which is not the usual case.
		t.Log("pid 1 is a php-fpm master here and was accepted")
	}

	// The property without needing to BE pid 1: identity decides, not the number.
	cmd := startSignalTarget(t, "trap ':' USR2", t.TempDir())
	if err := VerifyMaster(cmd.Process.Pid); err != nil {
		t.Errorf("a process carrying the master's title was refused: %v", err)
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

	_, err := ReloadAndWait(context.Background(), ReloadTarget{PID: cmd.Process.Pid}, 2*time.Second, nil)
	<-done

	if err == nil {
		t.Error("a master that exited during reload was reported as a success")
	}
}

// TestReloadAndWaitAcceptsAMasterThatSurvives is the happy path.
func TestReloadAndWaitAcceptsAMasterThatSurvives(t *testing.T) {
	cmd := startSignalTarget(t, "trap ':' USR2", t.TempDir())

	if _, err := ReloadAndWait(context.Background(), ReloadTarget{PID: cmd.Process.Pid}, 300*time.Millisecond, nil); err != nil {
		t.Errorf("a surviving master was reported as a failure: %v", err)
	}
}

func TestMasterPID(t *testing.T) {
	dir := t.TempDir()

	t.Run("reads a live pid", func(t *testing.T) {
		// A live php-fpm master rather than this test binary: a pid file is only
		// useful if what it names is still the master, and a stale one naming a
		// recycled pid is the failure this guards.
		master := startSignalTarget(t, "trap ':' USR2", t.TempDir())

		f := filepath.Join(dir, "live.pid")
		if err := os.WriteFile(f, []byte(strconv.Itoa(master.Process.Pid)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		pid, err := MasterPID(f)
		if err != nil {
			t.Fatalf("MasterPID: %v", err)
		}
		if pid != master.Process.Pid {
			t.Errorf("pid = %d, want %d", pid, master.Process.Pid)
		}
	})

	t.Run("rejects a pid file naming something that is not a master", func(t *testing.T) {
		// The stale-pid-file-plus-reuse case: the file survived the master, and
		// the number now belongs to something else.
		other := exec.Command("/bin/sh", "-c", "sleep 10")
		if err := other.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = other.Process.Kill()
			_, _ = other.Process.Wait()
		})

		f := filepath.Join(dir, "recycled.pid")
		if err := os.WriteFile(f, []byte(strconv.Itoa(other.Process.Pid)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := MasterPID(f); !errors.Is(err, ErrNotAMaster) {
			t.Errorf("a pid file naming an unrelated live process was accepted (err = %v)", err)
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

	// The no-op string at the front puts the master's process title into the
	// shell's command line, so the stub is recognised by the same check a real
	// master passes. Faking the check out instead would leave the one thing
	// standing between this package and SIGUSR2 to an arbitrary process
	// untested.
	title := `: "php-fpm: master process (/etc/php-fpm.conf)"; `
	cmd := exec.Command("/bin/sh", "-c", title+trap+"; touch "+ready+"; sleep 10 & wait")
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

// TestReloadAndWaitAcceptsAMasterThatCameBackWithANewPID.
//
// SIGUSR2 makes the master re-exec itself. In the foreground — under systemd, or
// as pid 1 in a container — the pid survives. DAEMONIZED, which is php-fpm's own
// default, the re-exec produces a new process and the original exits.
//
// Watching the original pid therefore reported a perfectly successful reload as
// a dead master. Observed on a stock homebrew php-fpm: the log said "using
// inherited socket" and "ready to handle connections" under a new pid while the
// caller rolled the change back and told the operator its master had died.
func TestReloadAndWaitAcceptsAMasterThatCameBackWithANewPID(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "fpm.pid")

	// Exits on USR2 after writing a SUCCESSOR's pid to the pid file, which is
	// what a daemonized reload looks like from outside.
	successorProc := startSignalTarget(t, "trap ':' USR2", t.TempDir())
	if err := os.WriteFile(pidFile,
		[]byte(strconv.Itoa(successorProc.Process.Pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	dying := startSignalTarget(t, "trap 'exit 0' USR2", t.TempDir())
	done := make(chan struct{})
	go func() { _, _ = dying.Process.Wait(); close(done) }()

	pid, err := ReloadAndWait(context.Background(),
		ReloadTarget{PID: dying.Process.Pid, PIDFile: pidFile}, 2*time.Second, nil)
	<-done

	if err != nil {
		t.Fatalf("a master that came back under a new pid was reported as dead: %v", err)
	}
	if pid != successorProc.Process.Pid {
		t.Errorf("pid = %d, want the successor %d", pid, successorProc.Process.Pid)
	}
}

// TestReloadAndWaitStillReportsAMasterWithNoSuccessor: the case above must not
// have made the real failure invisible.
func TestReloadAndWaitStillReportsAMasterWithNoSuccessor(t *testing.T) {
	dir := t.TempDir()

	dying := startSignalTarget(t, "trap 'exit 1' USR2", t.TempDir())
	done := make(chan struct{})
	go func() { _, _ = dying.Process.Wait(); close(done) }()

	_, err := ReloadAndWait(context.Background(), ReloadTarget{
		PID:     dying.Process.Pid,
		PIDFile: filepath.Join(dir, "does-not-exist.pid"),
	}, 500*time.Millisecond, nil)
	<-done

	if err == nil {
		t.Error("a master that died with nothing taking its place was reported as a success")
	}
}
