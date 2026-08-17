package objects

import (
	"bytes"
	"encoding/binary"
	"path/filepath"
	"reflect"
	"testing"

	"hived-go/internal/fcraw"
	"hived-go/internal/state"
)

// buildCommentSnapshotStream hand-assembles a comment_store snapshot stream
// (magic | version | count | (varuint|rec)*) from raw record byte slices, so
// tests can include legacy-schema (truncated) records the current encoder can
// no longer produce.
func buildCommentSnapshotStream(recs [][]byte) []byte {
	var buf bytes.Buffer
	buf.Write(commentStoreSnapshotMagic[:])
	var hdr [8]byte
	binary.LittleEndian.PutUint32(hdr[0:4], commentStoreSnapshotVersion)
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(len(recs)))
	buf.Write(hdr[:])
	var varBuf [binary.MaxVarintLen64]byte
	for _, rec := range recs {
		n := binary.PutUvarint(varBuf[:], uint64(len(rec)))
		buf.Write(varBuf[:n])
		buf.Write(rec)
	}
	return buf.Bytes()
}

// legacyCommentRec encodes ONLY the schema prefix up to and including WasPaid
// (no VoteRshares/root/... suffix) — the oldest spillable record shape. The
// loader must route it by the WasPaid byte and fall back to root=self.
func legacyCommentRec(author, permlink string, wasPaid bool) []byte {
	var dst []byte
	dst = fcraw.AppendVarUint(dst, uint32(len(author)))
	dst = append(dst, author...)
	dst = fcraw.AppendVarUint(dst, uint32(len(permlink)))
	dst = append(dst, permlink...)
	dst = fcraw.AppendVarUint(dst, 0)      // parent author
	dst = fcraw.AppendVarUint(dst, 0)      // parent permlink
	dst = append(dst, make([]byte, 36)...) // Created..AllowCurationRewards
	dst = fcraw.AppendVarUint(dst, 0)      // beneficiaries
	dst = append(dst, make([]byte, 8)...)  // AbsRshares
	if wasPaid {
		dst = append(dst, 1)
	} else {
		dst = append(dst, 0)
	}
	return dst
}

// TestSelfContainedSnapshotRawSpillLoad drives the raw-spill load path (peek
// routing + verbatim cold blob) end-to-end for both cold backends and both
// by-root modes, including legacy truncated records, and checks every comment
// (resident or promoted-from-cold) decodes identically to the record it was
// loaded from, with the cold bookkeeping (coldTotal/coldRoot) intact.
func TestSelfContainedSnapshotRawSpillLoad(t *testing.T) {
	type rec struct {
		bytes   []byte
		expect  Comment
		wantHot bool
	}
	mk := func(c Comment) []byte { return AppendComment(nil, &c) }
	modern := []Comment{
		{Author: "root1", Permlink: "post", NetRshares: 500, WasPaid: true,
			Beneficiaries: []Beneficiary{{Account: "ben1", Weight: 100}}},
		{Author: "replyer-a", Permlink: "re-post", RootAuthor: "root1", RootPermlink: "post",
			ParentAuthor: "root1", ParentPermlink: "post", NetRshares: 10, WasPaid: true},
		{Author: "root2", Permlink: "hello", NetRshares: 300, WasPaid: false},
		{Author: "bob", Permlink: "re-hello", RootAuthor: "root2", RootPermlink: "hello",
			ParentAuthor: "root2", ParentPermlink: "hello", NetRshares: 7, WasPaid: false},
	}
	var recs []rec
	for _, c := range modern {
		b := mk(c)
		exp, err := ReadComment(b)
		if err != nil {
			t.Fatal(err)
		}
		recs = append(recs, rec{bytes: b, expect: exp, wantHot: !c.WasPaid})
	}
	for _, lc := range []struct {
		author, permlink string
		paid             bool
	}{{"legacy", "old-paid", true}, {"legacy", "old-live", false}} {
		b := legacyCommentRec(lc.author, lc.permlink, lc.paid)
		exp, err := ReadComment(b)
		if err != nil {
			t.Fatal(err)
		}
		recs = append(recs, rec{bytes: b, expect: exp, wantHot: !lc.paid})
	}
	var raw [][]byte
	for _, r := range recs {
		raw = append(raw, r.bytes)
	}
	stream := buildCommentSnapshotStream(raw)

	for _, tc := range []struct {
		name    string
		byRoot  bool
		persist bool
	}{
		{"mmap-byroot", true, false},
		{"mmap-noroot", false, false},
		{"persist-byroot", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cold coldStore
			var err error
			if tc.persist {
				cold, err = OpenPersistCold(filepath.Join(t.TempDir(), "cold"))
			} else {
				cold, err = OpenColdTier(filepath.Join(t.TempDir(), "cold"), true)
			}
			if err != nil {
				t.Fatal(err)
			}
			db := openTempDB(t)
			cs := EnsureCommentStore(state.NewSession(db))
			cs.SetColdTier(cold)
			cs.SetResidentCap(1000) // spill=true, but nothing force-evicts
			cs.SetColdByRootEnabled(tc.byRoot)
			if err := cs.UnmarshalSnapshot(bytes.NewReader(stream)); err != nil {
				t.Fatal(err)
			}

			wantCold := 0
			for _, r := range recs {
				if !r.wantHot {
					wantCold++
				}
			}
			if cs.coldTotal != wantCold {
				t.Fatalf("coldTotal=%d want %d", cs.coldTotal, wantCold)
			}
			for _, r := range recs {
				key := newCommentPK(r.expect.Author, r.expect.Permlink)
				_, resident := cs.primary[key]
				if resident != r.wantHot {
					t.Fatalf("%s/%s resident=%v want %v", r.expect.Author, r.expect.Permlink, resident, r.wantHot)
				}
				got, ok := cs.Get(r.expect.Author, r.expect.Permlink)
				if !ok {
					t.Fatalf("%s/%s not found after load", r.expect.Author, r.expect.Permlink)
				}
				want := r.expect
				if r.wantHot {
					// Resident load applies the WasVotedOn reconstruction.
					want.WasVotedOn = want.WasVotedOn || want.AbsRshares != 0 || want.NetRshares != 0 || want.VoteRshares != 0
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("%s/%s mismatch after load:\n got %+v\nwant %+v", r.expect.Author, r.expect.Permlink, got, want)
				}
			}
			if tc.byRoot {
				// Cold per-root counts: paid records grouped under their
				// (fallback-resolved) roots.
				wantRoots := map[commentPK]int{}
				for _, r := range recs {
					if r.wantHot {
						continue
					}
					ra, rp := r.expect.RootAuthor, r.expect.RootPermlink
					if ra == "" {
						ra, rp = r.expect.Author, r.expect.Permlink
					}
					wantRoots[newCommentPK(ra, rp)]++
				}
				for k, want := range wantRoots {
					if cs.coldRoot[k] != want {
						t.Fatalf("coldRoot[%s/%s]=%d want %d", k.author, k.permlink, cs.coldRoot[k], want)
					}
				}
			}
		})
	}
}
