package phpfpm

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
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
	// old check let through. It even carries the master's title, because a
	// substring search on the command line accepted exactly this.
	cmd := exec.Command("/bin/sh", "-c",
		`: "php-fpm: master process (/etc/php-fpm.conf)"; sleep 10`)
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
	cmd := startSignalTarget(t, "ignore", t.TempDir())
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
	cmd := startNamedMaster(t, "marker", dir, "", marker).cmd

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
	cmd := startSignalTarget(t, "exit", t.TempDir())

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
	cmd := startSignalTarget(t, "ignore", t.TempDir())

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
		master := startSignalTarget(t, "ignore", t.TempDir())

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
func startSignalTarget(t *testing.T, onUSR2, dir string) *exec.Cmd {
	t.Helper()

	return startNamedMaster(t, onUSR2, dir, "", "").cmd
}

type stubMaster struct {
	cmd        *exec.Cmd
	configPath string
}

// startNamedMaster runs a process that passes the REAL identity check.
//
// VerifyMaster requires discovery-grade identity — the process name must match
// php-fpm's, and its executable must pass the same ownership checks discovery
// applies before running one — because a substring search on the command line is
// trivially spoofable, and a spoofed master is accepted as a SUCCESSOR: a local
// user could make a failed reload look survived, so nothing rolls back and the
// real pools stay down.
//
// So the stub is this test binary, copied under the name php-fpm and re-executed
// as a helper. Copying the system shell instead does not work — macOS kills a
// copy of a signed system binary — and faking the check out would leave the only
// thing standing between this package and SIGUSR2 to the wrong process
// unexercised.
func startNamedMaster(t *testing.T, onUSR2, dir, configPath, marker string) stubMaster {
	t.Helper()

	binDir := t.TempDir()
	binary := filepath.Join(binDir, "php-fpm")

	self, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Skipf("cannot read the test binary to build a stub master: %v", err)
	}
	if err := os.WriteFile(binary, self, 0o755); err != nil {
		t.Fatal(err)
	}

	if configPath == "" {
		configPath = filepath.Join(binDir, "php-fpm.conf")
		if err := os.WriteFile(configPath, []byte("[global]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ready := filepath.Join(dir, "ready")

	// The title goes in as an argument so it reaches the command line, which is
	// where the identity check reads it.
	cmd := exec.Command(binary, "-test.run=TestStubMasterHelper",
		"php-fpm: master process ("+configPath+")")
	cmd.Env = append(os.Environ(),
		"PHPFPM_STUB=1",
		"PHPFPM_STUB_READY="+ready,
		"PHPFPM_STUB_ON_USR2="+onUSR2,
		"PHPFPM_STUB_MARKER="+marker,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	// Waited for AFTER the handler is installed. The process exists the instant
	// it starts and the default action for USR2 is to terminate, so signalling
	// too early kills it and looks like a delivery failure.
	if !waitFor(t, 20*time.Second, func() bool {
		_, err := os.Stat(ready)

		return err == nil
	}) {
		t.Fatal("the stub master never signalled that its handler was installed")
	}

	return stubMaster{cmd: cmd, configPath: configPath}
}

// TestStubMasterHelper is the stub master, running inside a copy of this test
// binary. It is a no-op unless the environment says otherwise.
func TestStubMasterHelper(t *testing.T) {
	if os.Getenv("PHPFPM_STUB") != "1" {
		t.Skip("helper process only")
	}

	got := make(chan os.Signal, 1)
	signal.Notify(got, syscall.SIGUSR2)

	if ready := os.Getenv("PHPFPM_STUB_READY"); ready != "" {
		if err := os.WriteFile(ready, []byte("ok"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for {
		select {
		case <-got:
			switch os.Getenv("PHPFPM_STUB_ON_USR2") {
			case "exit":
				// Stands in for a master that cannot re-read its configuration.
				os.Exit(1)
			case "marker":
				if marker := os.Getenv("PHPFPM_STUB_MARKER"); marker != "" {
					_ = os.WriteFile(marker, []byte("yes"), 0o644)
				}
			}
		case <-time.After(30 * time.Second):
			return
		}
	}
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

	successorProc := startSignalTarget(t, "ignore", t.TempDir())
	dying := startSignalTarget(t, "exit", t.TempDir())

	// The pid file names the successor only AFTER the old master has gone, which
	// is the order a daemonized reload actually produces. Writing it up front
	// would let the watcher find the replacement without a reload having
	// happened at all — the test would then pass against a lookup that never
	// worked.
	done := make(chan struct{})
	go func() {
		_, _ = dying.Process.Wait()
		_ = os.WriteFile(pidFile, []byte(strconv.Itoa(successorProc.Process.Pid)+"\n"), 0o600)
		close(done)
	}()

	if _, err := os.Stat(pidFile); err == nil {
		t.Fatal("the pid file exists before the reload; the test would not prove anything")
	}

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

	dying := startSignalTarget(t, "exit", t.TempDir())
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

// TestReloadMasterRefusesTheWrongMaster.
//
// VerifyMaster on its own establishes that a pid is *a* php-fpm master. That is
// enough on a host running one and not enough on a host running several: a
// master can exit between discovery and the reload, and a pid handed straight
// back to a different master would be signalled as though it were ours —
// reloading someone else's sites into a configuration they never asked for.
//
// The config path is in the process title, so checking it costs nothing.
func TestReloadMasterRefusesTheWrongMaster(t *testing.T) {
	other := startNamedMaster(t, "ignore", t.TempDir(), "", "")

	if err := ReloadMaster(other.cmd.Process.Pid, "/etc/php/8.3/fpm/php-fpm.conf"); !errors.Is(err, ErrNotAMaster) {
		t.Errorf("a different master was reloaded as though it were ours (err = %v)", err)
	}

	// A substring match would accept this: the real path is a prefix of it.
	if err := ReloadMaster(other.cmd.Process.Pid, other.configPath+".old"); !errors.Is(err, ErrNotAMaster) {
		t.Errorf("%q was accepted for the master serving %q; the comparison is a "+
			"substring rather than a path (err = %v)", other.configPath+".old", other.configPath, err)
	}

	// And the one that does match is still accepted.
	if err := ReloadMaster(other.cmd.Process.Pid, other.configPath); err != nil {
		t.Errorf("the master serving the named configuration was refused: %v", err)
	}
}

// TestAReusedPidIsNotTheMasterThatWasSignalled.
//
// A pid is a small integer the kernel reuses. The settle window watched the
// NUMBER, so a master that died during it and had its pid taken — on a busy
// host, within the two seconds this watches for — was reported as having
// survived its reload, and the configuration that killed it was left in place
// for the next start to adopt.
//
// The start time is what makes a pid identify a process rather than a slot.
func TestAReusedPidIsNotTheMasterThatWasSignalled(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the start time is read from /proc, which only Linux has")
	}

	// This process, which is certainly alive and certainly started when it did.
	self := os.Getpid()
	started := processStartedAt(self)
	if started == 0 {
		t.Skip("no start time available for this process")
	}

	if !sameProcess(self, started) {
		t.Error("a process was not recognised as itself")
	}

	// A different start time is a different process wearing the same number.
	if sameProcess(self, started+1) {
		t.Error("a pid whose process started at a different moment was accepted as the " +
			"same one; that is the reused pid this exists to catch")
	}
}

// TestNoStartTimeMeansNoOpinion: off Linux, and on a Linux host where /proc
// cannot be read, the check has to stand aside rather than refuse. Failing a
// reload because a hint was unavailable would be worse than the reuse it
// guards against.
func TestNoStartTimeMeansNoOpinion(t *testing.T) {
	if !sameProcess(os.Getpid(), 0) {
		t.Error("a zero start time was treated as a mismatch; on a platform that cannot " +
			"read one, that refuses every reload")
	}
}

// TestABinaryFromAFileIsNotRunUnchecked.
//
// The trust check ran at DISCOVERY, on paths read out of the process table, and
// every other route to a binary went straight to exec. A caller can supply one
// from a file: fpm-tune remembers where php-fpm lives so it can repair a host
// with nothing running to discover, and that record is on disk. A state file an
// attacker can write is then a binary this process runs — usually as root.
//
// Checking at the point of USE rather than at the point of discovery is the
// difference between a guard on one path and a guard on the property.
func TestABinaryFromAFileIsNotRunUnchecked(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root owns everything, so the ownership check cannot be staged")
	}

	// A binary in a WORLD-writable directory without the sticky bit, which is
	// refused whether or not this runs as root. Running as root the check is
	// stricter still — every ancestor must be root-owned — but a test cannot
	// stage that as an ordinary user, and this shape is the one that matters
	// either way.
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "fake-php-fpm")
	if err := os.WriteFile(binary,
		[]byte("#!/bin/sh\ntouch "+filepath.Join(dir, "ran")+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	err := Validate(context.Background(), binary, filepath.Join(dir, "pwn.conf"))
	if err == nil {
		t.Fatal("a binary from a writable directory was executed")
	}
	if _, serr := os.Stat(filepath.Join(dir, "ran")); serr == nil {
		t.Error("it ran: a path out of a file reached exec without the trust check that " +
			"discovery applies to paths out of the process table")
	}
	if !strings.Contains(err.Error(), "refusing to run") {
		t.Errorf("the refusal does not say what it refused:\n%v", err)
	}
}
