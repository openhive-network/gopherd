package op

import (
	"unsafe"

	"hived-go/internal/crypto"
	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/asset"
	"hived-go/internal/protocol/auth"
)

// ---- 9 account_create ---------------------------------------------------

type AccountCreate struct {
	Fee            asset.Asset
	Creator        auth.AccountName
	NewAccountName auth.AccountName
	Owner          auth.Authority
	Active         auth.Authority
	Posting        auth.Authority
	MemoKey        crypto.PublicKey
	JSONMetadata   string
}

func (AccountCreate) OpTag() Tag { return TagAccountCreate }
func (o AccountCreate) AppendFCBody(dst []byte) []byte {
	dst = asset.AppendAsset(dst, o.Fee)
	dst = appendString(dst, o.Creator)
	dst = appendString(dst, o.NewAccountName)
	dst = auth.AppendAuthority(dst, o.Owner)
	dst = auth.AppendAuthority(dst, o.Active)
	dst = auth.AppendAuthority(dst, o.Posting)
	dst = append(dst, o.MemoKey[:]...)
	return appendString(dst, o.JSONMetadata)
}

func readAccountCreate(r *fcraw.Reader) (Op, error) {
	var o AccountCreate
	var err error
	if o.Fee, err = asset.ReadAsset(r); err != nil {
		return Op{}, err
	}
	if o.Creator, err = r.ReadString(); err != nil {
		return Op{}, err
	}
	if o.NewAccountName, err = r.ReadString(); err != nil {
		return Op{}, err
	}
	if o.Owner, err = auth.ReadAuthority(r); err != nil {
		return Op{}, err
	}
	if o.Active, err = auth.ReadAuthority(r); err != nil {
		return Op{}, err
	}
	if o.Posting, err = auth.ReadAuthority(r); err != nil {
		return Op{}, err
	}
	if err := r.ReadInto(o.MemoKey[:]); err != nil {
		return Op{}, err
	}
	if o.JSONMetadata, err = r.ReadString(); err != nil {
		return Op{}, err
	}
	return OpFromPtr(TagAccountCreate, unsafe.Pointer(&o)), nil
}

// ---- 10 account_update --------------------------------------------------

type AccountUpdate struct {
	Account      auth.AccountName
	Owner        *auth.Authority
	Active       *auth.Authority
	Posting      *auth.Authority
	MemoKey      crypto.PublicKey
	JSONMetadata string
}

func (AccountUpdate) OpTag() Tag { return TagAccountUpdate }
func (o AccountUpdate) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Account)
	dst = appendOptional(dst, o.Owner, auth.AppendAuthority)
	dst = appendOptional(dst, o.Active, auth.AppendAuthority)
	dst = appendOptional(dst, o.Posting, auth.AppendAuthority)
	dst = append(dst, o.MemoKey[:]...)
	return appendString(dst, o.JSONMetadata)
}

func readAccountUpdate(r *fcraw.Reader) (Op, error) {
	var o AccountUpdate
	var err error
	if o.Account, err = r.ReadString(); err != nil {
		return Op{}, err
	}
	if o.Owner, err = readOptional(r, auth.ReadAuthority); err != nil {
		return Op{}, err
	}
	if o.Active, err = readOptional(r, auth.ReadAuthority); err != nil {
		return Op{}, err
	}
	if o.Posting, err = readOptional(r, auth.ReadAuthority); err != nil {
		return Op{}, err
	}
	if err := r.ReadInto(o.MemoKey[:]); err != nil {
		return Op{}, err
	}
	if o.JSONMetadata, err = r.ReadString(); err != nil {
		return Op{}, err
	}
	return OpFromPtr(TagAccountUpdate, unsafe.Pointer(&o)), nil
}

// ---- 23 create_claimed_account ------------------------------------------

type CreateClaimedAccount struct {
	Creator        auth.AccountName
	NewAccountName auth.AccountName
	Owner          auth.Authority
	Active         auth.Authority
	Posting        auth.Authority
	MemoKey        crypto.PublicKey
	JSONMetadata   string
	Extensions     []byte
}

func (CreateClaimedAccount) OpTag() Tag { return TagCreateClaimedAccount }
func (o CreateClaimedAccount) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Creator)
	dst = appendString(dst, o.NewAccountName)
	dst = auth.AppendAuthority(dst, o.Owner)
	dst = auth.AppendAuthority(dst, o.Active)
	dst = auth.AppendAuthority(dst, o.Posting)
	dst = append(dst, o.MemoKey[:]...)
	dst = appendString(dst, o.JSONMetadata)
	return appendRawExtensions(dst, o.Extensions)
}

func readCreateClaimedAccount(r *fcraw.Reader) (Op, error) {
	var o CreateClaimedAccount
	var err error
	if o.Creator, err = r.ReadString(); err != nil {
		return Op{}, err
	}
	if o.NewAccountName, err = r.ReadString(); err != nil {
		return Op{}, err
	}
	if o.Owner, err = auth.ReadAuthority(r); err != nil {
		return Op{}, err
	}
	if o.Active, err = auth.ReadAuthority(r); err != nil {
		return Op{}, err
	}
	if o.Posting, err = auth.ReadAuthority(r); err != nil {
		return Op{}, err
	}
	if err := r.ReadInto(o.MemoKey[:]); err != nil {
		return Op{}, err
	}
	if o.JSONMetadata, err = r.ReadString(); err != nil {
		return Op{}, err
	}
	if o.Extensions, err = readExtensionsRaw(r, "extensions", futureExtensionsTags); err != nil {
		return Op{}, err
	}
	return OpFromPtr(TagCreateClaimedAccount, unsafe.Pointer(&o)), nil
}

// ---- 24 request_account_recovery ----------------------------------------

type RequestAccountRecovery struct {
	RecoveryAccount   auth.AccountName
	AccountToRecover  auth.AccountName
	NewOwnerAuthority auth.Authority
	Extensions        []byte
}

func (RequestAccountRecovery) OpTag() Tag { return TagRequestAccountRecovery }
func (o RequestAccountRecovery) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.RecoveryAccount)
	dst = appendString(dst, o.AccountToRecover)
	dst = auth.AppendAuthority(dst, o.NewOwnerAuthority)
	return appendRawExtensions(dst, o.Extensions)
}

func readRequestAccountRecovery(r *fcraw.Reader) (Op, error) {
	var o RequestAccountRecovery
	var err error
	if o.RecoveryAccount, err = r.ReadString(); err != nil {
		return Op{}, err
	}
	if o.AccountToRecover, err = r.ReadString(); err != nil {
		return Op{}, err
	}
	if o.NewOwnerAuthority, err = auth.ReadAuthority(r); err != nil {
		return Op{}, err
	}
	if o.Extensions, err = readExtensionsRaw(r, "extensions", futureExtensionsTags); err != nil {
		return Op{}, err
	}
	return OpFromPtr(TagRequestAccountRecovery, unsafe.Pointer(&o)), nil
}

// ---- 25 recover_account -------------------------------------------------

type RecoverAccount struct {
	AccountToRecover     auth.AccountName
	NewOwnerAuthority    auth.Authority
	RecentOwnerAuthority auth.Authority
	Extensions           []byte
}

func (RecoverAccount) OpTag() Tag { return TagRecoverAccount }
func (o RecoverAccount) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.AccountToRecover)
	dst = auth.AppendAuthority(dst, o.NewOwnerAuthority)
	dst = auth.AppendAuthority(dst, o.RecentOwnerAuthority)
	return appendRawExtensions(dst, o.Extensions)
}

func readRecoverAccount(r *fcraw.Reader) (Op, error) {
	var o RecoverAccount
	var err error
	if o.AccountToRecover, err = r.ReadString(); err != nil {
		return Op{}, err
	}
	if o.NewOwnerAuthority, err = auth.ReadAuthority(r); err != nil {
		return Op{}, err
	}
	if o.RecentOwnerAuthority, err = auth.ReadAuthority(r); err != nil {
		return Op{}, err
	}
	if o.Extensions, err = readExtensionsRaw(r, "extensions", futureExtensionsTags); err != nil {
		return Op{}, err
	}
	return OpFromPtr(TagRecoverAccount, unsafe.Pointer(&o)), nil
}

// ---- 26 change_recovery_account -----------------------------------------

type ChangeRecoveryAccount struct {
	AccountToRecover   auth.AccountName
	NewRecoveryAccount auth.AccountName
	Extensions         []byte
}

func (ChangeRecoveryAccount) OpTag() Tag { return TagChangeRecoveryAccount }
func (o ChangeRecoveryAccount) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.AccountToRecover)
	dst = appendString(dst, o.NewRecoveryAccount)
	return appendRawExtensions(dst, o.Extensions)
}

func readChangeRecoveryAccount(r *fcraw.Reader) (Op, error) {
	var o ChangeRecoveryAccount
	var err error
	if o.AccountToRecover, err = r.ReadString(); err != nil {
		return Op{}, err
	}
	if o.NewRecoveryAccount, err = r.ReadString(); err != nil {
		return Op{}, err
	}
	if o.Extensions, err = readExtensionsRaw(r, "extensions", futureExtensionsTags); err != nil {
		return Op{}, err
	}
	return OpFromPtr(TagChangeRecoveryAccount, unsafe.Pointer(&o)), nil
}

// ---- 41 account_create_with_delegation ----------------------------------

type AccountCreateWithDelegation struct {
	Fee            asset.Asset
	Delegation     asset.Asset
	Creator        auth.AccountName
	NewAccountName auth.AccountName
	Owner          auth.Authority
	Active         auth.Authority
	Posting        auth.Authority
	MemoKey        crypto.PublicKey
	JSONMetadata   string
	Extensions     []byte
}

func (AccountCreateWithDelegation) OpTag() Tag { return TagAccountCreateWithDelegation }
func (o AccountCreateWithDelegation) AppendFCBody(dst []byte) []byte {
	dst = asset.AppendAsset(dst, o.Fee)
	dst = asset.AppendAsset(dst, o.Delegation)
	dst = appendString(dst, o.Creator)
	dst = appendString(dst, o.NewAccountName)
	dst = auth.AppendAuthority(dst, o.Owner)
	dst = auth.AppendAuthority(dst, o.Active)
	dst = auth.AppendAuthority(dst, o.Posting)
	dst = append(dst, o.MemoKey[:]...)
	dst = appendString(dst, o.JSONMetadata)
	return appendRawExtensions(dst, o.Extensions)
}

func readAccountCreateWithDelegation(r *fcraw.Reader) (Op, error) {
	var o AccountCreateWithDelegation
	var err error
	if o.Fee, err = asset.ReadAsset(r); err != nil {
		return Op{}, err
	}
	if o.Delegation, err = asset.ReadAsset(r); err != nil {
		return Op{}, err
	}
	if o.Creator, err = r.ReadString(); err != nil {
		return Op{}, err
	}
	if o.NewAccountName, err = r.ReadString(); err != nil {
		return Op{}, err
	}
	if o.Owner, err = auth.ReadAuthority(r); err != nil {
		return Op{}, err
	}
	if o.Active, err = auth.ReadAuthority(r); err != nil {
		return Op{}, err
	}
	if o.Posting, err = auth.ReadAuthority(r); err != nil {
		return Op{}, err
	}
	if err := r.ReadInto(o.MemoKey[:]); err != nil {
		return Op{}, err
	}
	if o.JSONMetadata, err = r.ReadString(); err != nil {
		return Op{}, err
	}
	if o.Extensions, err = readExtensionsRaw(r, "extensions", futureExtensionsTags); err != nil {
		return Op{}, err
	}
	return OpFromPtr(TagAccountCreateWithDelegation, unsafe.Pointer(&o)), nil
}

// ---- 43 account_update2 -------------------------------------------------

type AccountUpdate2 struct {
	Account             auth.AccountName
	Owner               *auth.Authority
	Active              *auth.Authority
	Posting             *auth.Authority
	MemoKey             *crypto.PublicKey
	JSONMetadata        string
	PostingJSONMetadata string
	Extensions          []byte
}

func (AccountUpdate2) OpTag() Tag { return TagAccountUpdate2 }
func (o AccountUpdate2) AppendFCBody(dst []byte) []byte {
	dst = appendString(dst, o.Account)
	dst = appendOptional(dst, o.Owner, auth.AppendAuthority)
	dst = appendOptional(dst, o.Active, auth.AppendAuthority)
	dst = appendOptional(dst, o.Posting, auth.AppendAuthority)
	dst = appendOptional(dst, o.MemoKey, func(dst []byte, k crypto.PublicKey) []byte {
		return append(dst, k[:]...)
	})
	dst = appendString(dst, o.JSONMetadata)
	dst = appendString(dst, o.PostingJSONMetadata)
	return appendRawExtensions(dst, o.Extensions)
}

func readAccountUpdate2(r *fcraw.Reader) (Op, error) {
	var o AccountUpdate2
	var err error
	if o.Account, err = r.ReadString(); err != nil {
		return Op{}, err
	}
	if o.Owner, err = readOptional(r, auth.ReadAuthority); err != nil {
		return Op{}, err
	}
	if o.Active, err = readOptional(r, auth.ReadAuthority); err != nil {
		return Op{}, err
	}
	if o.Posting, err = readOptional(r, auth.ReadAuthority); err != nil {
		return Op{}, err
	}
	if o.MemoKey, err = readOptional(r, func(r *fcraw.Reader) (crypto.PublicKey, error) {
		var k crypto.PublicKey
		if err := r.ReadInto(k[:]); err != nil {
			return k, err
		}
		return k, nil
	}); err != nil {
		return Op{}, err
	}
	if o.JSONMetadata, err = r.ReadString(); err != nil {
		return Op{}, err
	}
	if o.PostingJSONMetadata, err = r.ReadString(); err != nil {
		return Op{}, err
	}
	if o.Extensions, err = readExtensionsRaw(r, "extensions", futureExtensionsTags); err != nil {
		return Op{}, err
	}
	return OpFromPtr(TagAccountUpdate2, unsafe.Pointer(&o)), nil
}

func init() {
	register(TagAccountCreate, readAccountCreate)
	register(TagAccountUpdate, readAccountUpdate)
	register(TagCreateClaimedAccount, readCreateClaimedAccount)
	register(TagRequestAccountRecovery, readRequestAccountRecovery)
	register(TagRecoverAccount, readRecoverAccount)
	register(TagChangeRecoveryAccount, readChangeRecoveryAccount)
	register(TagAccountCreateWithDelegation, readAccountCreateWithDelegation)
	register(TagAccountUpdate2, readAccountUpdate2)
}
