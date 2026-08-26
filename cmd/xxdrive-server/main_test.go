package main

import (
	"context"
	"testing"
	"time"
)

func TestAddrIsLoopback(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{":8080", false},            // empty host = all interfaces
		{":0", false},               //
		{"0.0.0.0:8080", false},     // explicit wildcard
		{"[::]:8080", false},        // IPv6 wildcard
		{"127.0.0.1:8080", true},    //
		{"127.8.8.8:80", true},      // whole 127/8 is loopback
		{"localhost:8080", true},    //
		{"LocalHost:8080", true},    // case-insensitive
		{"[::1]:8080", true},        //
		{"192.168.1.5:8080", false}, // LAN address
		{"10.0.0.1:80", false},      //
		{"", false},                 // fail closed on garbage
	}
	for _, tc := range cases {
		if got := addrIsLoopback(tc.addr); got != tc.want {
			t.Errorf("addrIsLoopback(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

// TestJanitorLoopStopsOnContextCancel verifies the janitor's stop path: after
// context cancellation the loop must return promptly (bounded well under a
// second) instead of waiting for the next tick.
func TestJanitorLoopStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	runs := 0
	go func() {
		defer close(done)
		janitorLoop(ctx, 20*time.Millisecond, func() { runs++ })
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("janitor did not stop after context cancellation")
	}
	if runs < 2 {
		t.Fatalf("janitor ran %d times over 100ms with a 20ms tick; want >= 2", runs)
	}
}
