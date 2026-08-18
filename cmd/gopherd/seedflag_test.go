package main

import "testing"

func TestRepeatableSeeds(t *testing.T) {
	var r repeatableSeeds
	r.Set("a:2001")
	r.Set("b:2001")
	if string(r) != "a:2001,b:2001" {
		t.Fatalf("repeat overwrote instead of appending: %q", r)
	}
	var c repeatableSeeds
	c.Set("a:2001,b:2001")
	c.Set(" ")
	c.Set("c:2001")
	if string(c) != "a:2001,b:2001,c:2001" {
		t.Fatalf("comma+repeat compose failed: %q", c)
	}
}
