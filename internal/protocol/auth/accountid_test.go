package auth

import (
	"sync"
	"testing"
)

func TestAccountIdInternRoundTrip(t *testing.T) {
	a := InternAccountId("alice")
	b := InternAccountId("bob")
	a2 := InternAccountId("alice")
	if a == InvalidAccountId || b == InvalidAccountId {
		t.Fatal("interned id must be nonzero")
	}
	if a != a2 {
		t.Fatalf("re-intern changed id: %d vs %d", a, a2)
	}
	if a == b {
		t.Fatal("distinct names share an id")
	}
	if got := AccountNameOf(a); got != "alice" {
		t.Fatalf("reverse(alice)=%q", got)
	}
	if got := AccountNameOf(b); got != "bob" {
		t.Fatalf("reverse(bob)=%q", got)
	}
	if got := AccountNameOf(InvalidAccountId); got != "" {
		t.Fatalf("reverse(0)=%q, want empty", got)
	}
	if _, ok := LookupAccountId("alice"); !ok {
		t.Fatal("lookup of interned name missed")
	}
	if _, ok := LookupAccountId("nobody-here"); ok {
		t.Fatal("lookup of unknown name should miss + not assign")
	}
	if _, ok := LookupAccountId("nobody-here"); ok {
		t.Fatal("LookupAccountId must not have assigned an id")
	}
}

// Concurrent interning must be race-free and assign exactly one id per name.
func TestAccountIdInternConcurrent(t *testing.T) {
	const names = 200
	var wg sync.WaitGroup
	ids := make([][]AccountId, 8)
	for w := 0; w < 8; w++ {
		ids[w] = make([]AccountId, names)
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < names; i++ {
				ids[w][i] = InternAccountId(concurrentName(i))
			}
		}(w)
	}
	wg.Wait()
	// Every worker must agree on the id for each name.
	for i := 0; i < names; i++ {
		want := ids[0][i]
		for w := 1; w < 8; w++ {
			if ids[w][i] != want {
				t.Fatalf("name %d: worker %d got %d, want %d", i, w, ids[w][i], want)
			}
		}
		if got := AccountNameOf(want); got != concurrentName(i) {
			t.Fatalf("reverse mismatch for %d: %q", i, got)
		}
	}
}

func concurrentName(i int) string {
	return "acct-" + string(rune('a'+i%26)) + string(rune('0'+i/26%10)) + string(rune('0'+i/260))
}
