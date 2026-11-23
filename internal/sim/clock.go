package sim

import (
	"hash/fnv"
	"strconv"
	"sync"
	"time"
)

// Clock abstracts time so the simulation can run on a virtual clock in tests.
type Clock interface {
	Now() time.Time
}

// RealClock follows wall-clock time, optionally sped up by Scale.
type RealClock struct {
	start time.Time
	base  time.Time
	Scale float64
}

// NewRealClock returns a clock that runs Scale times faster than real time.
func NewRealClock(scale float64) *RealClock {
	if scale <= 0 {
		scale = 1
	}
	now := time.Now()
	return &RealClock{start: now, base: now, Scale: scale}
}

// Now returns the (scaled) current time, truncated to seconds like the API server.
func (c *RealClock) Now() time.Time {
	elapsed := time.Since(c.start)
	return c.base.Add(time.Duration(float64(elapsed) * c.Scale)).Truncate(time.Second)
}

// FakeClock is a manually advanced clock for deterministic tests.
type FakeClock struct {
	mu sync.Mutex
	t  time.Time
}

// NewFakeClock creates a fake clock at t.
func NewFakeClock(t time.Time) *FakeClock { return &FakeClock{t: t} }

// Now returns the fake time.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance moves the fake clock forward.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// alphanums matches k8s.io/apimachinery/pkg/util/rand: no vowels, no
// confusable characters.
const alphanums = "bcdfghjklmnpqrstvwxz2456789"

func hash64(parts ...string) uint64 {
	h := fnv.New64a()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return h.Sum64()
}

// randString deterministically derives an n-character suffix.
func randString(seed uint64, n int) string {
	b := make([]byte, n)
	x := seed
	for i := 0; i < n; i++ {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		b[i] = alphanums[x%uint64(len(alphanums))]
	}
	return string(b)
}

// safeEncode mirrors rand.SafeEncodeString used for pod-template-hash.
func safeEncode(s string) string {
	r := make([]byte, len(s))
	for i, b := range []byte(s) {
		r[i] = alphanums[int(b)%len(alphanums)]
	}
	return string(r)
}

func templateHash(data []byte) string {
	h := fnv.New32a()
	_, _ = h.Write(data)
	return safeEncode(strconv.FormatUint(uint64(h.Sum32()), 10))
}
