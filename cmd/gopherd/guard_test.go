package main

import "testing"

// TestAPIEndpointIsLoopback pins the loopback/public classification that gates
// the API DoS guard. Anything not provably loopback must read as public.
func TestAPIEndpointIsLoopback(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8090", true},
		{"127.0.0.1", true},
		{"[::1]:8090", true},
		{"::1", true},
		{"localhost:8090", true},
		{"localhost", true},
		{"0.0.0.0:8090", false}, // all interfaces
		{":8090", false},        // empty host = all interfaces
		{"::", false},           // unspecified v6
		{"192.168.1.5:8090", false},
		{"10.0.0.2:8090", false},
		{"example.com:8090", false}, // unresolved hostname → public (safe default)
	}
	for _, c := range cases {
		if got := apiEndpointIsLoopback(c.addr); got != c.want {
			t.Errorf("apiEndpointIsLoopback(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}
