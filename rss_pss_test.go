package phpfpm

import "testing"

// TestParsePSSBytes: the total Pss is the bare "Pss:" line, in kB, and must not be
// confused with the Pss_Anon/Pss_File/Pss_Shmem breakdown that follows it. Getting
// that prefix match wrong would size pools from a fraction of the real cost.
func TestParsePSSBytes(t *testing.T) {
	// A real /proc/<pid>/smaps_rollup body, trimmed to the relevant lines.
	rollup := `55b0c0a00000-7ffd* ---p 00000000 00:00 0                          [rollup]
Rss:              148256 kB
Pss:               41984 kB
Pss_Dirty:         21000 kB
Pss_Anon:          20000 kB
Pss_File:          21984 kB
Pss_Shmem:             0 kB
Shared_Clean:     100000 kB
Private_Dirty:     20000 kB
`

	tests := []struct {
		name string
		in   string
		want int64
	}{
		{"real rollup takes the bare Pss line", rollup, 41984 * 1024},
		{"only Pss_Anon present is not a total", "Pss_Anon:  20000 kB\n", 0},
		{"missing Pss is zero", "Rss: 1000 kB\nShared_Clean: 500 kB\n", 0},
		{"garbage value is zero", "Pss:  not-a-number kB\n", 0},
		{"empty is zero", "", 0},
		{"zero is zero, not an error", "Pss:       0 kB\n", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parsePSSBytes([]byte(tt.in)); got != tt.want {
				t.Errorf("parsePSSBytes = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestPssOfIsSafeOnBadInput: an absent pid or a host without smaps_rollup (any
// non-Linux dev box, or a kernel before 4.14) must return 0, never panic, so the
// caller falls back to RSS.
func TestPssOfIsSafeOnBadInput(t *testing.T) {
	if got := pssOf(0); got != 0 {
		t.Errorf("pssOf(0) = %d, want 0", got)
	}
	if got := pssOf(-1); got != 0 {
		t.Errorf("pssOf(-1) = %d, want 0", got)
	}
	// A pid that does not exist: no /proc entry, must be 0.
	if got := pssOf(2 << 30); got != 0 {
		t.Errorf("pssOf(huge) = %d, want 0", got)
	}
}
