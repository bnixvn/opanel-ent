package main

import (
	"testing"
	"time"
)

// The two commands that install things need longer than the rest. A ten
// minute cap killed `cloudlinux setup` part-way through a package
// transaction, and reported it as "context deadline exceeded".
func TestCommandBudget(t *testing.T) {
	for _, tc := range []struct {
		args []string
		min  time.Duration
	}{
		{[]string{"cloudlinux", "setup"}, time.Hour},
		{[]string{"install"}, time.Hour},
		{[]string{"version"}, 0},
		{nil, 0},
	} {
		got := commandBudget(tc.args)
		if got < tc.min {
			t.Errorf("commandBudget(%v) = %s, want at least %s", tc.args, got, tc.min)
		}
		if got <= 0 {
			t.Errorf("commandBudget(%v) = %s, which would fail immediately", tc.args, got)
		}
	}
}
