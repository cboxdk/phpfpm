//go:build !linux

package phpfpm

import "github.com/shirou/gopsutil/v3/process"

// processStartedAt is the process's creation time, in milliseconds since the
// epoch, off Linux — where the /proc reading in start_linux.go is not available.
//
// It exists so the pid-reuse identity check in sameProcess is REAL off Linux, not
// a no-op. Returning a constant zero here — which is what this did — made
// sameProcess treat every pid as "no opinion" and pass: a reused pid was accepted
// as the master, and, worse, a bug that left the identity anchor pointing at a
// dead master went completely unseen on the machine this is developed on while it
// broke every reload on Linux. The unit differs from Linux's clock ticks, which
// does not matter: this value is only ever compared against another reading of
// itself on the same platform.
//
// Zero when it cannot be read, which sameProcess treats as "no opinion" rather
// than a mismatch — refusing a reload because a start time could not be read is
// worse than the reuse it guards against.
func processStartedAt(pid int) uint64 {
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return 0
	}

	created, err := p.CreateTime()
	if err != nil || created <= 0 {
		return 0
	}

	return uint64(created)
}
