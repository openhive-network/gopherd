package config

import (
	"strings"
	"testing"
)

func TestParseBasic(t *testing.T) {
	src := `
# this is a comment
witness = "alice"
witness = bob
private-key = 5JKey1
private-key = 5JKey2
plugin = chain p2p witness
data-dir = "/var/hived"
shared-file-size = 8G
p2p-endpoint = "0.0.0.0:2001"
`
	c, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := c.Witnesses; len(got) != 2 || got[0] != "alice" || got[1] != "bob" {
		t.Fatalf("Witnesses: got %v", got)
	}
	if got := c.PrivateKeys; len(got) != 2 || got[0] != "5JKey1" || got[1] != "5JKey2" {
		t.Fatalf("PrivateKeys: got %v", got)
	}
	if got := c.Plugins; len(got) != 3 || got[0] != "chain" || got[2] != "witness" {
		t.Fatalf("Plugins: got %v", got)
	}
	if c.DataDir != "/var/hived" {
		t.Fatalf("DataDir: got %q", c.DataDir)
	}
	if c.SharedFileSize != "8G" {
		t.Fatalf("SharedFileSize: got %q", c.SharedFileSize)
	}
	if c.P2PEndpoint != "0.0.0.0:2001" {
		t.Fatalf("P2PEndpoint: got %q", c.P2PEndpoint)
	}
}

func TestParseInlineCommentInQuote(t *testing.T) {
	src := `data-dir = "/path/with#hash"  # comment after`
	c, err := Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.DataDir != "/path/with#hash" {
		t.Fatalf("got %q", c.DataDir)
	}
}

func TestParseWitnessKeyMismatch(t *testing.T) {
	src := `
witness = alice
private-key = a
private-key = b
`
	if _, err := Parse(strings.NewReader(src)); err == nil {
		t.Fatal("expected error on mismatched count")
	}
}
