package chain

import (
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

// processAccountRecovery mirrors libraries/chain/database.cpp:1962
// account_recovery_processing. Two responsibilities:
//
//  1. Clear expired account_recovery_request rows.
//  2. Apply effective change_recovery_account_request rows (after the
//     30-day waiting period). Without an account_authority_object we
//     can't actually rotate the recovery_account; we just remove the
//     stale request row so the index doesn't accumulate.
//
// The owner_authority_history cleanup is omitted — we don't track that
// table since signature verification is skipped during replay.
func processAccountRecovery(s *state.Session, blockTime uint32) {
	clearExpiredRecoveryRequests(s, blockTime)
	applyEffectiveRecoveryAccountChanges(s, blockTime)
}

func clearExpiredRecoveryRequests(s *state.Session, blockTime uint32) {
	if hint, ok := s.GetExpHint(objects.AccountRecoveryRequestTable, objects.AccountRecoveryByExpirationIndex); ok && hint > blockTime {
		return
	}

	type pending struct {
		expiration uint32
		account    string
	}
	var ready []pending
	var nextExp uint32

	_ = s.Range(objects.AccountRecoveryRequestTable, objects.AccountRecoveryByExpirationIndex, nil,
		func(body, _ []byte) (bool, error) {
			if len(body) < 4+state.AccountNameKeyLen {
				return true, nil
			}
			t := uint32(body[0])<<24 | uint32(body[1])<<16 | uint32(body[2])<<8 | uint32(body[3])
			if t > blockTime {
				nextExp = t
				return false, nil
			}
			nameBytes := body[4 : 4+state.AccountNameKeyLen]
			n := state.AccountNameKeyLen
			for n > 0 && nameBytes[n-1] == 0 {
				n--
			}
			ready = append(ready, pending{expiration: t, account: string(nameBytes[:n])})
			return true, nil
		})

	for _, p := range ready {
		req, err := objects.GetAccountRecoveryRequest(s, p.account)
		if err != nil {
			continue
		}
		objects.DeleteAccountRecoveryRequestFull(s, &req)
	}
	if nextExp == 0 {
		nextExp = ^uint32(0)
	}
	s.SetExpHint(objects.AccountRecoveryRequestTable, objects.AccountRecoveryByExpirationIndex, nextExp)
}

func applyEffectiveRecoveryAccountChanges(s *state.Session, blockTime uint32) {
	if hint, ok := s.GetExpHint(objects.ChangeRecoveryAccountRequestTable, objects.ChangeRecoveryByEffectiveDateIndex); ok && hint > blockTime {
		return
	}

	type pending struct {
		effective uint32
		account   string
	}
	var ready []pending
	var nextExp uint32

	_ = s.Range(objects.ChangeRecoveryAccountRequestTable, objects.ChangeRecoveryByEffectiveDateIndex, nil,
		func(body, _ []byte) (bool, error) {
			if len(body) < 4+state.AccountNameKeyLen {
				return true, nil
			}
			t := uint32(body[0])<<24 | uint32(body[1])<<16 | uint32(body[2])<<8 | uint32(body[3])
			if t > blockTime {
				nextExp = t
				return false, nil
			}
			nameBytes := body[4 : 4+state.AccountNameKeyLen]
			n := state.AccountNameKeyLen
			for n > 0 && nameBytes[n-1] == 0 {
				n--
			}
			ready = append(ready, pending{effective: t, account: string(nameBytes[:n])})
			return true, nil
		})

	for _, p := range ready {
		// We don't have an account_authority_object, so just remove the
		// stale row to release the index slot. The actual recovery_account
		// rotation lands when authority objects do.
		v, err := s.Get(objects.ChangeRecoveryAccountRequestTable, state.PrimaryIndex,
			state.AppendAccountName(nil, p.account))
		if err != nil {
			continue
		}
		req, err := objects.ReadChangeRecoveryRequest(v)
		if err != nil {
			continue
		}
		objects.DeleteChangeRecoveryRequestFull(s, &req)
	}
	if nextExp == 0 {
		nextExp = ^uint32(0)
	}
	s.SetExpHint(objects.ChangeRecoveryAccountRequestTable, objects.ChangeRecoveryByEffectiveDateIndex, nextExp)
}
