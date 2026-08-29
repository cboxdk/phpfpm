//go:build linux

package phpfpm

import (
	"os/exec"
	"testing"
	"time"
)

// TestAZombieIsNotAliveMaster.
//
// A process that has exited and not yet been collected still answers signal 0,
// so the check that decides whether php-fpm survived its reload reported it
// healthy. Narrow — it needs the caller to be the process's parent, or an init
// that does not reap to have adopted it — but the whole job of that check is to
// tell a master that came back from one that did not, and on an unreaping PID 1
// the wrong answer is permanent.
func TestAZombieIsNotAliveMaster(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a helper process: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _, _ = cmd.Process.Wait() })

	// Deliberately not reaped: waiting is what turns it from a zombie into
	// nothing, and the zombie is the state under test.
	deadline := time.Now().Add(5 * time.Second)
	for !isZombie(pid) {
		if time.Now().After(deadline) {
			t.Skip("the helper never became a zombie; the host reaps for us")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if processAlive(pid) {
		t.Error("a zombie was reported as a running master: a reload that killed php-fpm " +
			"would be recorded as a success, and the configuration that killed it left " +
			"in place")
	}
}

// TestARunningProcessIsAlive is the other half, so a zombie check that simply
// answered false to everything could not pass.
func TestARunningProcessIsAlive(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "sleep 30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a helper process: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	if !processAlive(cmd.Process.Pid) {
		t.Error("a running process was reported as dead, which rolls back every good change")
	}
}
