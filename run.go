package phpfpm

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	// A relative name is resolved through PATH, and PATH is the attacker's
	// variable when this runs as root.
	if !filepath.IsAbs(binary) {
		return nil, fmt.Errorf("php binary must be an absolute path, got %q", binary)
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
