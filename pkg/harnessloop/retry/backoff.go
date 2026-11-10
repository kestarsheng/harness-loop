package retry

import (
	"math"
	"math/rand"
	"time"
)

type JitterFunc func() float64

func FixedJitter() float64 { return 0 }

func RandomJitter() float64 { return rand.Float64()*2 - 1 }

func (p Policy) BackoffFor(c Category, retryIndex int, jitter JitterFunc) time.Duration {
	if retryIndex < 1 {
		retryIndex = 1
	}
	if c == Quota || c == ContentFilter {
		return 0
	}
	base, ok := p.BackoffBase[c]
	if !ok || base <= 0 {
		return 0
	}
	factor := p.BackoffFactor
	if factor <= 0 {
		factor = 1
	}
	raw := float64(base) * math.Pow(factor, float64(retryIndex-1))
	if p.JitterRatio > 0 && jitter != nil {
		raw *= 1 + p.JitterRatio*jitter()
	}
	max := time.Duration(p.MaxBackoffMs) * time.Millisecond
	if max > 0 && raw > float64(max) {
		raw = float64(max)
	}
	if raw < 0 {
		return 0
	}
	return time.Duration(raw)
}

func ShouldBackoff(c Category) bool {
	return c != Quota && c != ContentFilter
}
