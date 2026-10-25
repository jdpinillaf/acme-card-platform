package accounts

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"time"

	"google.golang.org/grpc/codes"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	"github.com/acme/acme-card/pkg/grpcx"
)

type pageCursor struct {
	CreatedAt time.Time `json:"c"`
	ID        string    `json:"i"`
}

// ListTransactions returns the card's authorizations newest first, keyset-paginated.
func (s *Service) ListTransactions(ctx context.Context, req *accountsv1.ListTransactionsRequest) (*accountsv1.ListTransactionsResponse, error) {
	if _, err := s.load(ctx, s.pool, req.AccountId, false); err != nil {
		return nil, err
	}
	size := int(req.PageSize)
	if size <= 0 {
		size = 20
	}
	size = min(size, 100)
	cur := pageCursor{CreatedAt: time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), ID: "ffffffff-ffff-ffff-ffff-ffffffffffff"}
	if req.PageToken != "" {
		raw, err := base64.RawURLEncoding.DecodeString(req.PageToken)
		if err != nil || json.Unmarshal(raw, &cur) != nil || cur.ID == "" {
			return nil, grpcx.Error(codes.InvalidArgument, "invalid_page_token", "page_token is invalid")
		}
	}
	rows, err := s.pool.Query(ctx, `SELECT `+authCols+` FROM authorizations
		WHERE account_id=$1 AND (created_at, id) < ($2, $3::uuid)
		ORDER BY created_at DESC, id DESC LIMIT $4`, req.AccountId, cur.CreatedAt, cur.ID, size+1)
	if err != nil {
		return nil, internal(err)
	}
	defer rows.Close()
	var out []authRow
	for rows.Next() {
		a, err := scanAuth(rows)
		if err != nil {
			return nil, internal(err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, internal(err)
	}
	resp := &accountsv1.ListTransactionsResponse{}
	if len(out) > size {
		last := out[size-1]
		b, _ := json.Marshal(pageCursor{CreatedAt: last.CreatedAt, ID: last.ID})
		resp.NextPageToken = base64.RawURLEncoding.EncodeToString(b)
		out = out[:size]
	}
	for _, a := range out {
		resp.Authorizations = append(resp.Authorizations, a.proto())
	}
	return resp, nil
}
