//go:build !linux

package phpfpm

// isZombie has no portable answer off Linux, where this tool runs. Reporting
// false leaves the signal check as the only word, which is the behaviour that
// was there before.
func isZombie(_ int) bool { return false }
