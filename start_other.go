//go:build !linux

package phpfpm

// processStartedAt has no portable reading off Linux, where this runs. Zero
// means "no opinion", and sameProcess then falls back to the liveness and
// identity checks alone — which is the behaviour that was there before.
func processStartedAt(_ int) uint64 { return 0 }
