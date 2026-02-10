package entitlements

import (
	"math"
	"sync"
	"time"
)

type rateBucket struct {
	tokens       float64
	capacity     float64
	refillPerSec float64
	lastRefill   time.Time
}

type RateLimiter struct {
	now     func() time.Time
	mu      sync.Mutex
	buckets map[string]*rateBucket
}

func NewRateLimiter() *RateLimiter {
	return &RateLimiter{
		now:     func() time.Time { return time.Now().UTC() },
		buckets: make(map[string]*rateBucket),
	}
}

func (r *RateLimiter) Allow(orgID string, rpm int) (bool, int) {
	if rpm <= 0 || orgID == "" {
		return false, 60
	}

	now := r.now()
	capacity := float64(rpm)
	refillPerSec := capacity / 60.0

	r.mu.Lock()
	defer r.mu.Unlock()

	bucket, ok := r.buckets[orgID]
	if !ok {
		r.buckets[orgID] = &rateBucket{
			tokens:       capacity - 1,
			capacity:     capacity,
			refillPerSec: refillPerSec,
			lastRefill:   now,
		}
		return true, 0
	}

	elapsed := now.Sub(bucket.lastRefill).Seconds()
	if elapsed > 0 {
		bucket.tokens = math.Min(bucket.capacity, bucket.tokens+(elapsed*bucket.refillPerSec))
		bucket.lastRefill = now
	}
	if bucket.capacity != capacity || bucket.refillPerSec != refillPerSec {
		bucket.capacity = capacity
		bucket.refillPerSec = refillPerSec
		if bucket.tokens > bucket.capacity {
			bucket.tokens = bucket.capacity
		}
	}

	if bucket.tokens >= 1 {
		bucket.tokens -= 1
		return true, 0
	}

	deficit := 1 - bucket.tokens
	retrySeconds := int(math.Ceil(deficit / bucket.refillPerSec))
	if retrySeconds < 1 {
		retrySeconds = 1
	}
	return false, retrySeconds
}
