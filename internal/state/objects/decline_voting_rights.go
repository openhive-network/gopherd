package objects

import (
	"errors"
	"fmt"

	"hived-go/internal/fcraw"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
)

// DeclineVotingRightsRequestTable stores requests to revoke an
// account's voting rights. Created by decline_voting_rights_operation;
// the per-block process_decline_voting_rights hook applies the request
// after HIVE_OWNER_AUTH_RECOVERY_PERIOD (30 days).
const DeclineVotingRightsRequestTable state.TableID = 23

// DeclineVotingRightsByEffectiveIndex sorts requests by effective_date
// so the per-block hook scans in order.
const DeclineVotingRightsByEffectiveIndex state.IndexID = 1

// DeclineVotingRightsRequest mirrors hived's
// decline_voting_rights_request_object.
type DeclineVotingRightsRequest struct {
	Account       auth.AccountName
	EffectiveDate uint32
}

func declineVotingRightsKey(account auth.AccountName) []byte {
	return state.AppendAccountName(nil, account)
}

func declineVotingRightsByEffectiveKey(effective uint32, account auth.AccountName) []byte {
	out := state.AppendUint32BE(nil, effective)
	return state.AppendAccountName(out, account)
}

// AppendDeclineVotingRightsRequest serializes the request.
func AppendDeclineVotingRightsRequest(dst []byte, r *DeclineVotingRightsRequest) []byte {
	dst = fcraw.AppendVarUint(dst, uint32(len(r.Account)))
	dst = append(dst, r.Account...)
	dst = appendU32LE(dst, r.EffectiveDate)
	return dst
}

// ReadDeclineVotingRightsRequest decodes the request.
func ReadDeclineVotingRightsRequest(src []byte) (DeclineVotingRightsRequest, error) {
	r := fcraw.NewReader(src)
	var req DeclineVotingRightsRequest
	var err error
	if req.Account, err = r.ReadString(); err != nil {
		return req, fmt.Errorf("dvr.account: %w", err)
	}
	if req.EffectiveDate, err = r.ReadUint32(); err != nil {
		return req, fmt.Errorf("dvr.effective_date: %w", err)
	}
	if !r.AtEnd() {
		return req, errors.New("dvr: trailing bytes")
	}
	return req, nil
}

// PutDeclineVotingRightsRequest stores r and maintains the by-effective
// secondary index.
func PutDeclineVotingRightsRequest(s *state.Session, r *DeclineVotingRightsRequest) {
	s.Put(DeclineVotingRightsRequestTable, state.PrimaryIndex,
		declineVotingRightsKey(r.Account),
		AppendDeclineVotingRightsRequest(nil, r))
	s.Put(DeclineVotingRightsRequestTable, DeclineVotingRightsByEffectiveIndex,
		declineVotingRightsByEffectiveKey(r.EffectiveDate, r.Account), nil)
	if r.EffectiveDate != 0 {
		s.NudgeExpHint(DeclineVotingRightsRequestTable, DeclineVotingRightsByEffectiveIndex, r.EffectiveDate)
	}
}

// GetDeclineVotingRightsRequest fetches by account.
func GetDeclineVotingRightsRequest(s *state.Session, account auth.AccountName) (DeclineVotingRightsRequest, error) {
	v, err := s.Get(DeclineVotingRightsRequestTable, state.PrimaryIndex, declineVotingRightsKey(account))
	if err != nil {
		return DeclineVotingRightsRequest{}, err
	}
	return ReadDeclineVotingRightsRequest(v)
}

// HasDeclineVotingRightsRequest reports existence.
func HasDeclineVotingRightsRequest(s *state.Session, account auth.AccountName) (bool, error) {
	return s.Has(DeclineVotingRightsRequestTable, state.PrimaryIndex, declineVotingRightsKey(account))
}

// DeleteDeclineVotingRightsRequestFull removes both index rows.
func DeleteDeclineVotingRightsRequestFull(s *state.Session, r *DeclineVotingRightsRequest) {
	s.Delete(DeclineVotingRightsRequestTable, state.PrimaryIndex,
		declineVotingRightsKey(r.Account))
	s.Delete(DeclineVotingRightsRequestTable, DeclineVotingRightsByEffectiveIndex,
		declineVotingRightsByEffectiveKey(r.EffectiveDate, r.Account))
}
