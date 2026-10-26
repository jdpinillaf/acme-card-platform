package audit

import (
	"context"
	"log/slog"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	auditv1 "github.com/acme/acme-card/gen/go/acme/audit/v1"
)

// RunDaily runs the daily integrity jobs (checkpoint; reconciliation and retention are added by
// their tasks) once per UTC day, checking every interval.
func (s *Service) RunDaily(ctx context.Context, every time.Duration, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	last := ""
	for {
		if day := s.now().Format("2006-01-02"); day != last {
			_, err := s.CreateCheckpoint(ctx, &auditv1.CreateCheckpointRequest{})
			switch {
			case err == nil:
				last = day
			case status.Code(err) == codes.FailedPrecondition: // empty chain: retry next interval
			case ctx.Err() == nil:
				log.Error("daily checkpoint failed", "err", err)
			}
			for _, job := range s.dailyJobs {
				if err := job(ctx); err != nil && ctx.Err() == nil {
					log.Error("daily audit job failed", "err", err)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
