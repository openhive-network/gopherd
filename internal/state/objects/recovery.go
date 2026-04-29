package objects

import (
	"errors"

	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
)

// AccountRecoveryRequest is the account_recovery_request_object — a
// pending request to rotate an account's owner authority. Settles or
// expires after HIVE_ACCOUNT_RECOVERY_REQUEST_EXPIRATION_PERIOD (24h).
//
// We capture the new owner authority as raw bytes; full authority
// rotation lands with the account_authority_object schema.
type AccountRecoveryRequest struct {
	AccountToRecover    auth.AccountName
	NewOwnerAuthorityRaw []byte
	Expiration          uint32
}

// ChangeRecoveryAccountRequest tracks a pending change of the recovery
// account, with a HIVE_OWNER_AUTH_RECOVERY_PERIOD (30 day) waiting period.
type ChangeRecoveryAccountRequest struct {
	AccountToRecover   auth.AccountName
	RecoveryAccount    auth.AccountName
	EffectiveOn        uint32
}

const (
	AccountRecoveryRequestTable        state.TableID = 14
	ChangeRecoveryAccountRequestTable  state.TableID = 15
)

func recoveryRequestKey(account auth.AccountName) []byte {
	return state.AppendAccountName(nil, account)
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
	s.Put(AccountRecoveryRequestTable, state.PrimaryIndex, recoveryRequestKey(r.AccountToRecover), AppendAccountRecoveryRequest(nil, r))
}

func DeleteAccountRecoveryRequest(s *state.Session, account auth.AccountName) {
	s.Delete(AccountRecoveryRequestTable, state.PrimaryIndex, recoveryRequestKey(account))
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
	s.Put(ChangeRecoveryAccountRequestTable, state.PrimaryIndex, recoveryRequestKey(r.AccountToRecover), AppendChangeRecoveryRequest(nil, r))
}
