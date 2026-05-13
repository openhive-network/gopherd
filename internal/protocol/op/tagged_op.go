package op

import (
	"unsafe"

	"hived-go/internal/fcraw"
)

// Op is a tagged-union representation of a regular operation, replacing
// the `Operation` interface dispatch on the hot path. The benefits over
// the interface:
//
//   - Tag is read directly from the struct instead of via a method call
//     (no itab indirection on OpTag()).
//   - Callers can switch on Tag and access the concrete value via the
//     typed `AsX()` helper, avoiding the type-assertion that pays an
//     itab compare on every iface read.
//   - Op is 16 bytes (Tag + ptr) — same as an interface — so the change
//     is alloc-neutral when the underlying concrete value is heap-boxed,
//     and becomes a clean win when paired with per-type pooling for
//     the concrete payload (future iteration).
//
// Migration approach: this type lives alongside the `Operation` interface
// during the migration. Each callsite is converted independently. Once
// all 30+ consumer files are migrated, `Operation` becomes deprecated
// and is removed.
type Op struct {
	Tag Tag
	// ptr points to the concrete op struct. The concrete type is
	// determined by Tag — callers use the matching AsX() helper.
	ptr unsafe.Pointer
}

// OpTag returns the tag of this Op. Provided as a method (in addition to
// the Tag field) so callers can write `o.OpTag()` interchangeably with
// the Operation interface during migration. Inlines to a field load.
func (o Op) OpTag() Tag { return o.Tag }

// AppendFCBody encodes the operation body to dst. Bridges to the boxed
// Operation form so Op satisfies the Operation interface while
// consumers are still on the iface API.
func (o Op) AppendFCBody(dst []byte) []byte {
	if o.ptr == nil {
		return dst
	}
	return o.Operation().AppendFCBody(dst)
}

// FromOperation wraps an Operation iface into an Op. The concrete value
// stays heap-boxed via the iface; this constructor is used as a bridge
// during the iface→struct migration. Once all readers populate Op
// directly, FromOperation is no longer needed.
func FromOperation(o Operation) Op {
	return Op{Tag: o.OpTag(), ptr: ifacePtr(o)}
}

// ifacePtr extracts the data-pointer from an interface value. The Go
// interface layout is [itab, data]; the second word is the data pointer.
//
//go:nosplit
func ifacePtr(o Operation) unsafe.Pointer {
	type iface struct {
		_   unsafe.Pointer
		ptr unsafe.Pointer
	}
	return (*iface)(unsafe.Pointer(&o)).ptr
}

// OpFromPtr constructs an Op directly from a tag and the concrete
// struct's address. Used by readers that allocate the concrete struct
// and want to publish it via Op without a per-op iface box.
func OpFromPtr(tag Tag, ptr unsafe.Pointer) Op {
	return Op{Tag: tag, ptr: ptr}
}

// Operation returns the boxed iface form of this Op, for callsites still
// expecting the Operation interface. Cost: one iface box per call, same
// shape as the legacy path. Remove call sites incrementally.
func (o Op) Operation() Operation {
	if o.ptr == nil {
		return nil
	}
	// The fact that we know the concrete type from Tag lets us cast
	// without a runtime lookup. The dispatch table makes it explicit.
	return opAsIface[o.Tag](o.ptr)
}

// Ops is a variadic constructor for a []Op from Operation literals.
// Useful in tests that previously wrote []op.Operation{op.Vote{...}, ...}
// — they can now write op.Ops(op.Vote{...}, ...) and the call site
// stays terse. Each Operation is wrapped via FromOperation.
func Ops(ops ...Operation) []Op {
	out := make([]Op, len(ops))
	for i, o := range ops {
		out[i] = FromOperation(o)
	}
	return out
}

// opAsIface is the per-tag conversion table: given a concrete-struct
// pointer for that tag, return the boxed Operation. Populated by init().
var opAsIface [TagFirstVirtual]func(unsafe.Pointer) Operation

// ReadOp parses a single tagged operation from r and returns it as Op.
// Bridge wrapper around ReadOperation during the iface→struct migration:
// uses the existing readers (which already populate slab-pooled storage
// where applicable) and then wraps in Op via FromOperation so callers
// no longer touch the Operation interface directly.
func ReadOp(r *fcraw.Reader) (Op, error) {
	o, err := ReadOperation(r)
	if err != nil {
		return Op{}, err
	}
	return FromOperation(o), nil
}

// AppendOp writes a tagged operation as varint(tag) || body using the
// Op container's concrete pointer. Bridge form of AppendOperation.
func AppendOp(dst []byte, o Op) []byte {
	if o.ptr == nil {
		return dst
	}
	return AppendOperation(dst, o.Operation())
}
