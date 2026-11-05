package gateway

import "testing"

func TestShapePriority(t *testing.T) {
	for _, c := range []struct {
		minMs float64
		want  int
	}{{0, prioReport}, {300, prioPage}, {1000, prioPage}, {1001, prioReport}, {42000, prioReport}} {
		sh := &shape{minMs: c.minMs}
		if got := sh.priority(); got != c.want {
			t.Errorf("fastest %.0f ms: priority %d, want %d", c.minMs, got, c.want)
		}
	}
}
