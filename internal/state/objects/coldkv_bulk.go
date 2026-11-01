package objects

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cockroachdb/pebble/objstorage/objstorageprovider"
	"github.com/cockroachdb/pebble/sstable"
	"github.com/cockroachdb/pebble/vfs"
)

// Bulk load: a self-contained snapshot's cold comments go into the pebble
// tier as SSTs built directly and ingested, instead of as batched writes.
//
// Batched writes cost the whole LSM pipeline per row — memtable, flush to L0,
// then compaction rewriting the data level by level. On the measured
// mainnet load that was ~310 GB written for a 26 GB corpus at 19 MB/s, while
// reading and parsing the same file ran at 348 MB/s. An ingested SST is
// written once and placed at the lowest level it fits, so the spill costs
// about one sequential write of the corpus.
//
// Ingestion needs each file's keys strictly increasing. The cold half of a
// snapshot is streamed out of pebble in key order, so it arrives sorted; the
// resident half (sorted on its own) and anything else form separate runs.
// A key at or below the previous one ends the current run: its files are
// ingested before the key is written, and ingestion gives them sequence
// numbers above everything already in the tier, so later rows still win over
// earlier ones exactly as with batched writes.
//
// Only the comment primary table is bulk-loaded. Every other table (the
// pre-HF17 by-root index, which arrives in comment order rather than its own
// key order) keeps the batch path; the tables never share keys, so mixing
// the two cannot reorder writes to the same key.

// bulkSSTTarget is the size at which a run's current SST is closed and a new
// one started. Large enough to keep the file count low, small enough that a
// run's pending files are ingested in reasonable steps.
var bulkSSTTarget uint64 = 256 << 20

type coldBulk struct {
	dir     string // scratch dir for SSTs before ingestion (inside the tier dir)
	wopts   sstable.WriterOptions
	w       *sstable.Writer
	path    string
	pending []string
	last    []byte
	seq     int
	rows    int64
	runs    int
}

// beginBulkLoad switches comment-primary writes to SST ingestion until
// endBulkLoad.
func (c *coldKV) beginBulkLoad() error {
	if c.bulk != nil {
		return nil
	}
	dir := filepath.Join(c.dir, "bulk-ingest")
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	o := c.opts.Clone().EnsureDefaults()
	c.bulk = &coldBulk{
		dir:   dir,
		wopts: o.MakeWriterOptions(0, c.db.FormatMajorVersion().MaxTableFormat()),
	}
	return nil
}

// endBulkLoad ingests whatever is pending and returns to batched writes.
func (c *coldKV) endBulkLoad() error {
	b := c.bulk
	if b == nil {
		return nil
	}
	err := c.ingestRun()
	c.bulk = nil
	_ = os.RemoveAll(b.dir)
	if err != nil {
		c.failWrite(err)
		return err
	}
	if b.rows > 0 {
		fmt.Fprintf(os.Stderr, "cold tier: bulk-loaded %d rows as SSTs in %d ingest run(s)\n", b.rows, b.runs)
	}
	return nil
}

// bulkSet writes one comment-primary row through the bulk path.
func (c *coldKV) bulkSet(key, val []byte) {
	b := c.bulk
	if b.last != nil && bytes.Compare(key, b.last) <= 0 {
		if err := c.ingestRun(); err != nil {
			c.failWrite(err)
			return
		}
	}
	if b.w == nil {
		b.path = filepath.Join(b.dir, fmt.Sprintf("%06d.sst", b.seq))
		b.seq++
		f, err := vfs.Default.Create(b.path)
		if err != nil {
			c.failWrite(err)
			return
		}
		b.w = sstable.NewWriter(objstorageprovider.NewFileWritable(f), b.wopts)
	}
	if err := b.w.Set(key, val); err != nil {
		c.failWrite(err)
		return
	}
	b.last = append(b.last[:0], key...)
	b.rows++
	if b.w.EstimatedSize() >= bulkSSTTarget {
		if err := c.finishSST(); err != nil {
			c.failWrite(err)
		}
	}
}

func (c *coldKV) finishSST() error {
	b := c.bulk
	if b.w == nil {
		return nil
	}
	err := b.w.Close()
	b.w = nil
	if err != nil {
		return err
	}
	b.pending = append(b.pending, b.path)
	return nil
}

// ingestRun closes the current SST and ingests the run's files.
func (c *coldKV) ingestRun() error {
	b := c.bulk
	if err := c.finishSST(); err != nil {
		return err
	}
	b.last = b.last[:0]
	if len(b.pending) == 0 {
		return nil
	}
	// Rows written by the batch path so far must sit below this run.
	c.flushBatch()
	if err := c.db.Ingest(b.pending); err != nil {
		return fmt.Errorf("cold bulk load: ingest: %w", err)
	}
	for _, p := range b.pending {
		_ = os.Remove(p) // pebble linked (or copied) it into the tier
	}
	b.pending = b.pending[:0]
	b.runs++
	return nil
}

// failWrite records a cold write failure stickily, as flushBatch does.
func (c *coldKV) failWrite(err error) {
	if c.writeErr == nil {
		c.writeErr = err
		fmt.Fprintf(os.Stderr, "COLD TIER WRITE FAILED — evicted rows are LOST and will read as missing: %v\n", err)
	}
}

// coldBulkLoader is implemented by cold tiers with a bulk-load path.
type coldBulkLoader interface {
	beginBulkLoad() error
	endBulkLoad() error
}

var _ coldBulkLoader = (*coldKV)(nil)
