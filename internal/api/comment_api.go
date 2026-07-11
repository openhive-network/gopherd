package api

import (
	"context"
	"encoding/json"
	"strconv"

	"hived-go/internal/api/jsonrpc"
	"hived-go/internal/protocol/auth"
	"hived-go/internal/state"
	"hived-go/internal/state/objects"
)

func (s *Server) registerCommentAPI() {
	s.reg.Register("condenser_api.get_active_votes", s.mGetActiveVotes())
}

// mGetActiveVotes: condenser get_active_votes [author, permlink] → the votes on a
// comment. reputation is 0 (needs the reputation index — Phase E).
func (s *Server) mGetActiveVotes() jsonrpc.Handler {
	return func(_ context.Context, params json.RawMessage) (interface{}, error) {
		var arr []json.RawMessage
		if json.Unmarshal(params, &arr) != nil || len(arr) < 2 {
			return nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "expected [author, permlink]")
		}
		var author, permlink string
		_ = json.Unmarshal(arr[0], &author)
		_ = json.Unmarshal(arr[1], &permlink)
		out := []map[string]interface{}{}
		_ = s.chain.WithReadSession(func(sess *state.Session) error {
			return objects.RangeCommentVotesByComment(sess, auth.AccountName(author), permlink,
				func(cv *objects.CommentVote) (bool, error) {
					out = append(out, map[string]interface{}{
						"voter":      string(cv.Voter),
						"weight":     strconv.FormatUint(cv.CurationWeight, 10),
						"rshares":    cv.Rshares,
						"percent":    cv.Weight,
						"time":       timeString(cv.LastUpdate),
						"reputation": 0,
					})
					return true, nil
				})
		})
		return out, nil
	}
}
