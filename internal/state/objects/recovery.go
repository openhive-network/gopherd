package objects

import (
	"errors"

	"gopherd/internal/fcraw"
	"gopherd/internal/protocol/auth"
	"gopherd/internal/state"
)

// AccountRecoveryRequest is the account_recovery_request_object — a
// pending request to rotate an account's owner authority. Settles or
// expires after HIVE_ACCOUNT_RECOVERY_REQUEST_EXPIRATION_PERIOD (24h).
//
// We capture the new owner authority as raw bytes; full authority
// rotation lands with the account_authority_object schema.
type AccountRecoveryRequest struct {
	AccountToRecover     auth.AccountName
	NewOwnerAuthorityRaw []byte
	Expiration           uint32
}

// ChangeRecoveryAccountRequest tracks a pending change of the recovery
// account, with a HIVE_OWNER_AUTH_RECOVERY_PERIOD (30 day) waiting period.
type ChangeRecoveryAccountRequest struct {
	AccountToRecover auth.AccountName
	RecoveryAccount  auth.AccountName
	EffectiveOn      uint32
}

const (
	AccountRecoveryRequestTable       state.TableID = 14
	ChangeRecoveryAccountRequestTable state.TableID = 15
	// Secondary indices for the per-block account_recovery_processing
	// hook (libraries/chain/database.cpp:1962): expired
	// account_recovery_request rows get cleared, and pending
	// change_recovery_account rows fire when their effective time hits.
	AccountRecoveryByExpirationIndex   state.IndexID = 1
	ChangeRecoveryByEffectiveDateIndex state.IndexID = 1
)

func recoveryRequestKey(account auth.AccountName) []byte {
	return state.AppendAccountName(nil, account)
}

func accountRecoveryByExpirationKey(expiration uint32, account auth.AccountName) []byte {
	out := state.AppendUint32BE(nil, expiration)
	return state.AppendAccountName(out, account)
}

func changeRecoveryByEffectiveDateKey(effective uint32, account auth.AccountName) []byte {
	out := state.AppendUint32BE(nil, effective)
	return state.AppendAccountName(out, account)
}

func AppendAccountRecoveryRequest(dst []byte, r *AccountRecoveryRequest) []byte {
	dst = fcraw.AppendVarUint(dst, uint32(len(r.AccountToRecover)))
	dst = append(dst, r.AccountToRecover...)
	dst = fcraw.AppendVarUint(dst, uint32(len(r.NewOwnerAuthorityRaw)))
	dst = append(dst, r.NewOwnerAuthorityRaw...)
	dst = appendU32LE(dst, r.Expiration)
	return dst
}

func ReadAccountRecoveryRequest(src []byte) (AccountRecoveryRequest, error) {
	r := fcraw.NewReader(src)
	var rr AccountRecoveryRequest
	a, err := r.ReadString()
	if err != nil {
		return rr, err
	}
	rr.AccountToRecover = a
	if rr.NewOwnerAuthorityRaw, err = r.ReadBytes(); err != nil {
		return rr, err
	}
	if rr.Expiration, err = r.ReadUint32(); err != nil {
		return rr, err
	}
	if !r.AtEnd() {
		return rr, errors.New("account_recovery_request: trailing bytes")
	}
	return rr, nil
}

func PutAccountRecoveryRequest(s *state.Session, r *AccountRecoveryRequest) {
	// If a row already exists with a DIFFERENT expiration, the old
	// secondary-index entry must be removed first — otherwise the
	// (expiration, account) ring accumulates orphan entries that fire
	// the sweep at stale timestamps. The primary-index entry is
	// upserted by the second Put below.
	if prev, err := s.Get(AccountRecoveryRequestTable, state.PrimaryIndex, recoveryRequestKey(r.AccountToRecover)); err == nil {
		if prevRow, perr := ReadAccountRecoveryRequest(prev); perr == nil && prevRow.Expiration != r.Expiration {
			s.Delete(AccountRecoveryRequestTable, AccountRecoveryByExpirationIndex,
				accountRecoveryByExpirationKey(prevRow.Expiration, prevRow.AccountToRecover))
		}
	}
	s.Put(AccountRecoveryRequestTable, state.PrimaryIndex, recoveryRequestKey(r.AccountToRecover), AppendAccountRecoveryRequest(nil, r))
	s.Put(AccountRecoveryRequestTable, AccountRecoveryByExpirationIndex,
		accountRecoveryByExpirationKey(r.Expiration, r.AccountToRecover), nil)
	// Lower the expiration hint so the per-block sweep can't skip this
	// newly-inserted row. Without it, a sweep that previously found nothing
	// pending leaves the hint at max and this request becomes invisible
	// forever (every other expiring-table Put — savings, escrow,
	// decline_voting_rights, vesting_delegation_expiration — does this).
	s.NudgeExpHint(AccountRecoveryRequestTable, AccountRecoveryByExpirationIndex, r.Expiration)
}

func DeleteAccountRecoveryRequest(s *state.Session, account auth.AccountName) {
	// Caller is responsible for clearing the secondary entry too if it
	// has the row in hand (see DeleteAccountRecoveryRequestFull).
	s.Delete(AccountRecoveryRequestTable, state.PrimaryIndex, recoveryRequestKey(account))
}

// DeleteAccountRecoveryRequestFull removes both indices.
func DeleteAccountRecoveryRequestFull(s *state.Session, r *AccountRecoveryRequest) {
	s.Delete(AccountRecoveryRequestTable, state.PrimaryIndex, recoveryRequestKey(r.AccountToRecover))
	s.Delete(AccountRecoveryRequestTable, AccountRecoveryByExpirationIndex,
		accountRecoveryByExpirationKey(r.Expiration, r.AccountToRecover))
}

func GetAccountRecoveryRequest(s *state.Session, account auth.AccountName) (AccountRecoveryRequest, error) {
	v, err := s.Get(AccountRecoveryRequestTable, state.PrimaryIndex, recoveryRequestKey(account))
	if err != nil {
		return AccountRecoveryRequest{}, err
	}
	return ReadAccountRecoveryRequest(v)
}

func HasAccountRecoveryRequest(s *state.Session, account auth.AccountName) (bool, error) {
	return s.Has(AccountRecoveryRequestTable, state.PrimaryIndex, recoveryRequestKey(account))
}

func AppendChangeRecoveryRequest(dst []byte, r *ChangeRecoveryAccountRequest) []byte {
	dst = fcraw.AppendVarUint(dst, uint32(len(r.AccountToRecover)))
	dst = append(dst, r.AccountToRecover...)
	dst = fcraw.AppendVarUint(dst, uint32(len(r.RecoveryAccount)))
	dst = append(dst, r.RecoveryAccount...)
	dst = appendU32LE(dst, r.EffectiveOn)
	return dst
}

func ReadChangeRecoveryRequest(src []byte) (ChangeRecoveryAccountRequest, error) {
	r := fcraw.NewReader(src)
	var rr ChangeRecoveryAccountRequest
	a, err := r.ReadString()
	if err != nil {
		return rr, err
	}
	rr.AccountToRecover = a
	if rr.RecoveryAccount, err = r.ReadString(); err != nil {
		return rr, err
	}
	if rr.EffectiveOn, err = r.ReadUint32(); err != nil {
		return rr, err
	}
	return rr, nil
}

func PutChangeRecoveryRequest(s *state.Session, r *ChangeRecoveryAccountRequest) {
	// Same secondary-index orphan fix as PutAccountRecoveryRequest:
	// delete the old by_effective_date entry if expiration changes.
	if prev, err := s.Get(ChangeRecoveryAccountRequestTable, state.PrimaryIndex, recoveryRequestKey(r.AccountToRecover)); err == nil {
		if prevRow, perr := ReadChangeRecoveryRequest(prev); perr == nil && prevRow.EffectiveOn != r.EffectiveOn {
			s.Delete(ChangeRecoveryAccountRequestTable, ChangeRecoveryByEffectiveDateIndex,
				changeRecoveryByEffectiveDateKey(prevRow.EffectiveOn, prevRow.AccountToRecover))
		}
	}
	s.Put(ChangeRecoveryAccountRequestTable, state.PrimaryIndex, recoveryRequestKey(r.AccountToRecover), AppendChangeRecoveryRequest(nil, r))
	s.Put(ChangeRecoveryAccountRequestTable, ChangeRecoveryByEffectiveDateIndex,
		changeRecoveryByEffectiveDateKey(r.EffectiveOn, r.AccountToRecover), nil)
	// Lower the effective-date hint so applyEffectiveRecoveryAccountChanges
	// can't skip this newly-inserted request. Without it, a sweep that
	// previously found nothing pending leaves the hint at max and the change
	// never matures — account.recovery_account stays stale (paidsteemvoter
	// kept "steem" instead of rotating to "pz13"; request_account_recovery
	// then failed at block 18,481,234).
	s.NudgeExpHint(ChangeRecoveryAccountRequestTable, ChangeRecoveryByEffectiveDateIndex, r.EffectiveOn)
}

// DeleteChangeRecoveryRequestFull removes both indices.
func DeleteChangeRecoveryRequestFull(s *state.Session, r *ChangeRecoveryAccountRequest) {
	s.Delete(ChangeRecoveryAccountRequestTable, state.PrimaryIndex, recoveryRequestKey(r.AccountToRecover))
	s.Delete(ChangeRecoveryAccountRequestTable, ChangeRecoveryByEffectiveDateIndex,
		changeRecoveryByEffectiveDateKey(r.EffectiveOn, r.AccountToRecover))
}

// GetChangeRecoveryRequest fetches a pending change_recovery_account
// request by account name. Returns ErrNotFound if no pending request
// exists.
func GetChangeRecoveryRequest(s *state.Session, account auth.AccountName) (ChangeRecoveryAccountRequest, error) {
	v, err := s.Get(ChangeRecoveryAccountRequestTable, state.PrimaryIndex, recoveryRequestKey(account))
	if err != nil {
		return ChangeRecoveryAccountRequest{}, err
	}
	return ReadChangeRecoveryRequest(v)
}

// HasChangeRecoveryRequest reports whether a pending request exists.
func HasChangeRecoveryRequest(s *state.Session, account auth.AccountName) (bool, error) {
	return s.Has(ChangeRecoveryAccountRequestTable, state.PrimaryIndex, recoveryRequestKey(account))
}
