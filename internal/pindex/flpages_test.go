package pindex

import (
	"testing"
	"time"
)

// The old fixed-point loop for the free-list page count never terminated for
// R>=2 and R+O == flCap+1 (it held t.mu forever: all other goroutines blocked).
func TestFlPagesTerminates(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for R := 0; R <= 4; R++ {
			for _, tot := range []int{0, 1, flCap - 1, flCap, flCap + 1, flCap + 2, 2*flCap + 1, 2*flCap + 2} {
				if tot < R {
					continue
				}
				O := tot - R
				k := flPages(R, O)
				if k*flCap < R+O-min(k, R) {
					t.Errorf("R=%d O=%d: k=%d too small", R, O, k)
				}
				if k > 0 && (R+O-min(k-1, R)+flCap-1)/flCap <= k-1 {
					t.Errorf("R=%d O=%d: k=%d not minimal", R, O, k)
				}
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("flPages did not terminate")
	}
}
