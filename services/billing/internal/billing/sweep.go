package billing

import (
	"context"
	"log/slog"
	"time"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
)

// SweepOnce catches up every active account once; real time passing moves account time too.
func (s *Service) SweepOnce(ctx context.Context, log *slog.Logger) (int, error) {
	after, n := "", 0
	for {
		r, err := s.accounts.ListAccountIDs(ctx, &accountsv1.ListAccountIDsRequest{AfterId: after, Limit: 500})
		if err != nil {
			return n, err
		}
		if len(r.Ids) == 0 {
			return n, nil
		}
		for _, id := range r.Ids {
			if ctx.Err() != nil {
				return n, ctx.Err()
			}
			if _, err := s.CatchUpAccount(ctx, id); err != nil {
				log.Warn("billing catch-up failed; retried next sweep", "account_id", id, "err", err)
				continue
			}
			n++
		}
		after = r.Ids[len(r.Ids)-1]
	}
}

// RunSweep runs SweepOnce every interval until ctx ends.
func (s *Service) RunSweep(ctx context.Context, every time.Duration, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if _, err := s.SweepOnce(ctx, log); err != nil && ctx.Err() == nil {
			log.Error("billing sweep failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
