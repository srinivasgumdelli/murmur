package handler

import (
	"testing"
	"time"
)

func TestClaimLimiterWindow(t *testing.T) {
	now := time.Date(2026, time.November, 27, 9, 0, 0, 0, time.UTC)
	limiter := &claimLimiter{limit: 2, window: 10 * time.Minute, now: func() time.Time { return now }}
	if !limiter.allow("a") || !limiter.allow("a") {
		t.Fatal("limiter refused claims within the limit")
	}
	if limiter.allow("a") {
		t.Fatal("limiter accepted a claim over the limit")
	}
	if !limiter.allow("b") {
		t.Fatal("one source's limit blocked another source")
	}
	now = now.Add(10 * time.Minute)
	if !limiter.allow("a") {
		t.Fatal("limiter did not recover after the window")
	}
}
