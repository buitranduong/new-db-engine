package sql

import (
	"math"
	"math/bits"
	"sync"
	"time"
)

// pgPRNG is PostgreSQL 16 pg_prng: xoroshiro128** seeded through splitmix64.
// setseed(x) seeds it exactly as PostgreSQL does, so random() after the same
// setseed returns the same values as PostgreSQL (verified by pgdiff).
type pgPRNG struct {
	mu     sync.Mutex
	s0, s1 uint64
	seeded bool
}

func splitmix64(s *uint64) uint64 {
	*s += 0x9E3779B97F4A7C15
	z := *s
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

func (p *pgPRNG) seed(seed uint64) {
	p.s0 = splitmix64(&seed)
	p.s1 = splitmix64(&seed)
	if p.s0 == 0 && p.s1 == 0 {
		p.s0 = 1
	}
	p.seeded = true
}

// setseed seeds from a double in [-1, 1].
func (p *pgPRNG) setseed(f float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seed(uint64(int64(float64(uint64(1)<<52-1) * f)))
}

func (p *pgPRNG) float() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.seeded {
		p.seed(uint64(time.Now().UnixNano()))
	}
	sx := p.s1 ^ p.s0
	v := bits.RotateLeft64(p.s0*5, 7) * 9
	p.s0 = bits.RotateLeft64(p.s0, 24) ^ sx ^ (sx << 16)
	p.s1 = bits.RotateLeft64(sx, 37)
	return math.Ldexp(float64(v>>12), -52)
}

// sessionRand is the generator of c's session (the process-wide one when
// there is none).
func (c *evalCtx) sessionRand() *pgPRNG {
	if s := c.session(); s != nil {
		return &s.prng
	}
	return &globalPRNG
}

var globalPRNG pgPRNG
