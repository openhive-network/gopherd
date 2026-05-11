package op

import (
	"encoding/binary"
	"fmt"

	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/auth"
)

// ---- 1 comment ----------------------------------------------------------

type Comment struct {
	ParentAuthor   auth.AccountName
	ParentPermlink string
	Author         auth.AccountName
	Permlink       string
	Title          string
	Body           string
	JSONMetadata   string
}

func (Comment) OpTag() Tag { return TagComment }
func (o Comment) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.ParentAuthor)
	dst = appendString(dst, o.ParentPermlink)
	dst = appendString(dst, o.Author)
	dst = appendString(dst, o.Permlink)
	dst = appendString(dst, o.Title)
	dst = appendString(dst, o.Body)
	return appendString(dst, o.JSONMetadata)
}

func readComment(r *fcraw.Reader) (Operation, error) {
	var o Comment
	var err error
	if o.ParentAuthor, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.ParentPermlink, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.Author, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.Permlink, err = r.ReadString(); err != nil {
		return nil, err
	}
	// Title / Body / JSONMetadata are never inspected by chain logic and
	// never survive into the typed CommentStore (it doesn't carry these
	// fields). ReadStringAlias gives a zero-copy string into the source
	// buffer — safe because the parsed op is fully consumed inside the
	// block's evaluator.Apply, before the next BlockReuse reuses the
	// buffer.
	if o.Title, err = r.ReadStringAlias(); err != nil {
		return nil, err
	}
	if o.Body, err = r.ReadStringAlias(); err != nil {
		return nil, err
	}
	if o.JSONMetadata, err = r.ReadStringAlias(); err != nil {
		return nil, err
	}
	return o, nil
}

// ---- 19 comment_options -------------------------------------------------

// CommentBeneficiary is a single (account, weight) entry inside the
// comment_payout_beneficiaries variant of comment_options_extensions.
type CommentBeneficiary struct {
	Account auth.AccountName
	Weight  uint16
}

// CommentOptions captures the operation. Extensions are decoded into the
// strongly-typed Beneficiaries slice — the only variant currently defined
// is `comment_payout_beneficiaries` (tag 0). When present, Extensions is
// also captured as raw bytes for round-trip parity.
type CommentOptions struct {
	Author               auth.AccountName
	Permlink             string
	MaxAcceptedPayout    asset.Asset
	PercentHbd           uint16
	AllowVotes           bool
	AllowCurationRewards bool
	// RawExtensions preserves the on-wire bytes of the extensions field so
	// re-marshal is bit-identical even for variant tags we add later.
	RawExtensions []byte
	// Beneficiaries is parsed from the
	// comment_payout_beneficiaries variant (tag 0). Empty when no extension
	// of that tag is present.
	Beneficiaries []CommentBeneficiary
}

func (CommentOptions) OpTag() Tag { return TagCommentOptions }
func (o CommentOptions) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Author)
	dst = appendString(dst, o.Permlink)
	dst = asset.AppendAsset(dst, o.MaxAcceptedPayout)
	dst = binary.LittleEndian.AppendUint16(dst, o.PercentHbd)
	if o.AllowVotes {
		dst = append(dst, 1)
	} else {
		dst = append(dst, 0)
	}
	if o.AllowCurationRewards {
		dst = append(dst, 1)
	} else {
		dst = append(dst, 0)
	}
	// Prefer raw byte preservation (set when this op was decoded from the
	// wire) so we round-trip bit-identically; otherwise reconstruct from
	// the typed Beneficiaries slice.
	if len(o.RawExtensions) > 0 {
		return append(dst, o.RawExtensions...)
	}
	if len(o.Beneficiaries) == 0 {
		return append(dst, 0) // empty extensions
	}
	dst = fcraw.AppendVarUint(dst, 1) // count = 1 (single comment_payout_beneficiaries)
	dst = fcraw.AppendVarUint(dst, 0) // variant tag 0
	dst = fcraw.AppendVarUint(dst, uint32(len(o.Beneficiaries)))
	for _, b := range o.Beneficiaries {
		dst = appendString(dst, b.Account)
		dst = binary.LittleEndian.AppendUint16(dst, b.Weight)
	}
	return dst
}

func readCommentOptions(r *fcraw.Reader) (Operation, error) {
	var o CommentOptions
	var err error
	if o.Author, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.Permlink, err = r.ReadString(); err != nil {
		return nil, err
	}
	if o.MaxAcceptedPayout, err = asset.ReadAsset(r); err != nil {
		return nil, err
	}
	if o.PercentHbd, err = r.ReadUint16(); err != nil {
		return nil, err
	}
	if o.AllowVotes, err = r.ReadBool(); err != nil {
		return nil, err
	}
	if o.AllowCurationRewards, err = r.ReadBool(); err != nil {
		return nil, err
	}

	// Extensions: flat_set<comment_options_extension>.
	// Variants:
	//   tag 0: comment_payout_beneficiaries — vector<beneficiary_route_type>
	//          where beneficiary_route_type = (account_name, weight uint16).
	extStart := r.Pos()
	count, err := r.ReadVarUint()
	if err != nil {
		return nil, fmt.Errorf("comment_options.extensions count: %w", err)
	}
	for i := uint32(0); i < count; i++ {
		tag, err := r.ReadVarUint()
		if err != nil {
			return nil, fmt.Errorf("comment_options.extensions[%d] tag: %w", i, err)
		}
		switch tag {
		case 0:
			// comment_payout_beneficiaries
			n, err := r.ReadVarUint()
			if err != nil {
				return nil, fmt.Errorf("beneficiaries count: %w", err)
			}
			for j := uint32(0); j < n; j++ {
				name, err := r.ReadString()
				if err != nil {
					return nil, fmt.Errorf("beneficiary[%d] account: %w", j, err)
				}
				w, err := r.ReadUint16()
				if err != nil {
					return nil, fmt.Errorf("beneficiary[%d] weight: %w", j, err)
				}
				o.Beneficiaries = append(o.Beneficiaries, CommentBeneficiary{Account: name, Weight: w})
			}
		default:
			return nil, fmt.Errorf("comment_options.extensions[%d]: unknown variant tag %d", i, tag)
		}
	}
	extEnd := r.Pos()
	o.RawExtensions = append([]byte(nil), r.SourceRange(extStart, extEnd)...)

	return o, nil
}

func init() {
	register(TagComment, readComment)
	register(TagCommentOptions, readCommentOptions)
}
