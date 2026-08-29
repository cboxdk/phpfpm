package phpfpm

import (
	"strings"
	"testing"
)

// TestAResponseThatIsNotThisPoolsStatusPageIsRefused.
//
// The decode alone accepted anything shaped like JSON. An empty object became a
// zero-valued Pool with no error at all — a live site reported as running no
// workers and having served nothing, which a caller that divides memory between
// pools reads as a site nobody is using, and whose share it gives away.
//
// A status path pointing at an application rather than at php-fpm's own status
// page is a configuration mistake, not an exotic one; so is one pointing at
// another pool's socket, and that second case is worse — one site's numbers get
// measured and another site's ceiling gets written from them.
func TestAResponseThatIsNotThisPoolsStatusPageIsRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		reported, asked string
		wantErr         bool
		mentions        string
	}{
		"nothing named at all":      {"", "shop", true, "status page"},
		"another pool's page":       {"forum", "shop", true, "forum"},
		"the pool that was asked":   {"shop", "shop", false, ""},
		"a spelling difference":     {"Shop", "shop", false, ""},
		"no pool asked for by name": {"shop", "", false, ""},
	} {
		t.Run(name, func(t *testing.T) {
			err := checkIsStatusFor(tc.reported, tc.asked, "/status")
			if tc.wantErr && err == nil {
				t.Fatalf("a response reporting %q was accepted for %q", tc.reported, tc.asked)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("a real status page was refused: %v", err)
			}
			if tc.mentions != "" && !strings.Contains(err.Error(), tc.mentions) {
				t.Errorf("the error does not mention %q: %v", tc.mentions, err)
			}
		})
	}
}
