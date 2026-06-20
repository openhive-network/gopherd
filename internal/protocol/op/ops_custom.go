package op

import (
	"unsafe"

	"encoding/binary"
	"fmt"

	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/auth"
)

// readSortedAccountSet reads a flat_set<account_name_type> as varint count +
// each name; preserves on-wire order so re-marshal is bit-identical.
func readSortedAccountSet(r *fcraw.Reader, label string) ([]auth.AccountName, error) {
	count, err := r.ReadVarUint()
	if err != nil {
		return nil, fmt.Errorf("%s count: %w", label, err)
	}
	out := make([]auth.AccountName, 0, count)
	for i := uint32(0); i < count; i++ {
		name, err := r.ReadString()
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: %w", label, i, err)
		}
		out = append(out, name)
	}
	return out, nil
}

func appendSortedAccountSet(dst []byte, names []auth.AccountName) []byte {
	dst = fcraw.AppendVarUint(dst, uint32(len(names)))
	for _, n := range names {
		dst = appendString(dst, n)
	}
	return dst
}

// ---- 15 custom ----------------------------------------------------------

type Custom struct {
	RequiredAuths []auth.AccountName
	ID            uint16
	Data          []byte
}

func (Custom) OpTag() Tag { return TagCustom }
func (o Custom) AppendFCBody(dst []byte) []byte {
	dst = appendSortedAccountSet(dst, o.RequiredAuths)
	dst = binary.LittleEndian.AppendUint16(dst, o.ID)
	dst = fcraw.AppendVarUint(dst, uint32(len(o.Data)))
	return append(dst, o.Data...)
}

func readCustom(r *fcraw.Reader) (Op, error) {
	var o Custom
	var err error
	if o.RequiredAuths, err = readSortedAccountSet(r, "custom.required_auths"); err != nil {
		return Op{}, err
	}
	if o.ID, err = r.ReadUint16(); err != nil {
		return Op{}, err
	}
	if o.Data, err = r.ReadBytes(); err != nil {
		return Op{}, err
	}
	return OpFromPtr(TagCustom, unsafe.Pointer(&o)), nil
}

// ---- 18 custom_json -----------------------------------------------------

type CustomJSON struct {
	RequiredAuths        []auth.AccountName
	RequiredPostingAuths []auth.AccountName
	ID                   string // custom_id_type = fixed_string<32>; serialized as std::string
	JSON                 string
}

func (CustomJSON) OpTag() Tag { return TagCustomJSON }
func (o CustomJSON) AppendFCBody(dst []byte) []byte {
	dst = appendSortedAccountSet(dst, o.RequiredAuths)
	dst = appendSortedAccountSet(dst, o.RequiredPostingAuths)
	dst = appendString(dst, o.ID)
	return appendString(dst, o.JSON)
}

func readCustomJSON(r *fcraw.Reader) (Op, error) {
	var o CustomJSON
	var err error
	if o.RequiredAuths, err = readSortedAccountSet(r, "custom_json.required_auths"); err != nil {
		return Op{}, err
	}
	if o.RequiredPostingAuths, err = readSortedAccountSet(r, "custom_json.required_posting_auths"); err != nil {
		return Op{}, err
	}
	if o.ID, err = r.ReadString(); err != nil {
		return Op{}, err
	}
	// JSON is validated-then-discarded (the only consumers, custom_json_rc /
	// rc_differential, json.Unmarshal a fresh []byte copy and are RC-gated).
	// Alias it into the block buffer like Comment.Body/Title/JSONMetadata —
	// the op struct never outlives ApplyBlock. ~3.3GB/60s of dense-era
	// (bidbot/custom_json spam) allocation eliminated → fewer GC cycles.
	if o.JSON, err = r.ReadStringAlias(); err != nil {
		return Op{}, err
	}
	return OpFromPtr(TagCustomJSON, unsafe.Pointer(&o)), nil
}

// ---- 35 custom_binary ---------------------------------------------------

type CustomBinary struct {
	RequiredOwnerAuths   []auth.AccountName
	RequiredActiveAuths  []auth.AccountName
	RequiredPostingAuths []auth.AccountName
	RequiredAuths        []auth.Authority
	ID                   string
	Data                 []byte
}

func (CustomBinary) OpTag() Tag { return TagCustomBinary }
func (o CustomBinary) AppendFCBody(dst []byte) []byte {
	dst = appendSortedAccountSet(dst, o.RequiredOwnerAuths)
	dst = appendSortedAccountSet(dst, o.RequiredActiveAuths)
	dst = appendSortedAccountSet(dst, o.RequiredPostingAuths)
	dst = appendAuthoritySlice(dst, o.RequiredAuths)
	dst = appendString(dst, o.ID)
	dst = fcraw.AppendVarUint(dst, uint32(len(o.Data)))
	return append(dst, o.Data...)
}

func readCustomBinary(r *fcraw.Reader) (Op, error) {
	var o CustomBinary
	var err error
	if o.RequiredOwnerAuths, err = readSortedAccountSet(r, "custom_binary.required_owner_auths"); err != nil {
		return Op{}, err
	}
	if o.RequiredActiveAuths, err = readSortedAccountSet(r, "custom_binary.required_active_auths"); err != nil {
		return Op{}, err
	}
	if o.RequiredPostingAuths, err = readSortedAccountSet(r, "custom_binary.required_posting_auths"); err != nil {
		return Op{}, err
	}
	if o.RequiredAuths, err = readAuthoritySlice(r, "custom_binary.required_auths"); err != nil {
		return Op{}, err
	}
	if o.ID, err = r.ReadString(); err != nil {
		return Op{}, err
	}
	if o.Data, err = r.ReadBytes(); err != nil {
		return Op{}, err
	}
	return OpFromPtr(TagCustomBinary, unsafe.Pointer(&o)), nil
}

func init() {
	register(TagCustom, readCustom)
	register(TagCustomJSON, readCustomJSON)
	register(TagCustomBinary, readCustomBinary)
}
