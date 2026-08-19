package main

import "strings"

// repeatableSeeds accumulates --extra-seed instead of overwriting it.
//
// The flag was a plain StringVar whose help told you to "pass --extra-seed
// multiple times", which silently kept only the LAST one: flag.StringVar
// assigns, it does not append. Only the undocumented comma form actually
// pinned more than one peer.
//
// That mattered more than a cosmetic help fix. Pinning specific peers is the
// only lever measured to move inbound transaction latency, and the two peers
// supplying 33% of our earliest adverts are NOT compiled-in seeds — they are
// peer-exchange finds. Losing them to a silently-dropped flag degrades block
// fullness in a way that looks like random network variance.
//
// Both forms now work, and they compose:
//
//	--extra-seed a:2001 --extra-seed b:2001
//	--extra-seed a:2001,b:2001
type repeatableSeeds string

func (r *repeatableSeeds) String() string { return string(*r) }

func (r *repeatableSeeds) Set(v string) error {
	if v = strings.TrimSpace(v); v == "" {
		return nil
	}
	if *r == "" {
		*r = repeatableSeeds(v)
		return nil
	}
	*r = repeatableSeeds(string(*r) + "," + v)
	return nil
}
