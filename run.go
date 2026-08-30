package phpfpm

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// maxCapturedOutput bounds what is read back from a forked php-fpm.
//
// A binary that will not stop talking — a wrapper script in a loop, a
// configuration include that shells out — otherwise fills memory in a process
// that is supposed to be a small supervisor.
const maxCapturedOutput = 4 << 20

// runBounded forks a php-fpm binary and returns its combined output.
//
// Three things it does that exec.CommandContext alone does not, and each of
// them was a real failure before it existed in one place:
//
// The child gets its own PROCESS GROUP and the group is killed on cancellation.
// Killing only the child leaves anything it started holding the pipe, so the
// read blocks on a dead parent's descendants and the caller's timeout buys
// nothing.
//
// The output is BOUNDED. CombinedOutput reads until EOF, which is however much
// the child cares to write.
//
// And the caller's CONTEXT is honoured, rather than a background one invented
// here — a scrape loop with a two-second budget should not be able to sit for
// thirty inside a call it made.
func runBounded(ctx context.Context, binary string, args ...string) ([]byte, error) {
	// The full trust check, HERE, where the exec happens.
	//
	// It used to run only at discovery, on paths read out of the process table —
	// and every other route to a binary went straight to exec. A caller can
	// supply one from a file: fpm-tune remembers where php-fpm lives so it can
	// repair a host with nothing running to discover, and that record is on
	// disk. A state file an attacker can write is then a binary this process
	// runs, usually as root.
	//
	// Checking at the point of USE rather than at the point of discovery is the
	// difference between a guard on one path and a guard on the property. The
	// cost is an Lstat and a walk up the directory chain, a few times per round.
	if err := trustedPath(binary); err != nil {
		return nil, fmt.Errorf("refusing to run %s: %w", binary, err)
	}

	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}

		return os.ErrProcessDone
	}

	var captured bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &captured, remaining: maxCapturedOutput}
	cmd.Stderr = cmd.Stdout

	err := cmd.Run()

	return captured.Bytes(), err
}
