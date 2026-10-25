package audit

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	auditv1 "github.com/acme/acme-card/gen/go/acme/audit/v1"
)

// Verify recomputes the chain, checks every signed checkpoint, lists per-source gaps and stuck
// producer outboxes. A full run walks the whole chain and is recorded; otherwise the walk starts
// after the last recorded full run, whose anchor hash must still be in place (a re-hash of
// anything before it changes that hash).
func (s *Service) Verify(ctx context.Context, req *auditv1.VerifyRequest) (*auditv1.VerifyReport, error) {
	r := &auditv1.VerifyReport{}
	from, prev, err := s.chainStart(ctx)
	if err != nil {
		return nil, internal(err)
	}
	var bad, checked int64
	if !req.GetFull() {
		if bad, from, prev, err = s.resumeFromFullRun(ctx, r, from, prev); err != nil {
			return nil, internal(err)
		}
	}
	if bad == 0 {
		bad, checked, err = s.store.VerifyFrom(ctx, from, 0, prev)
		if err != nil && bad == 0 {
			return nil, internal(err)
		}
	}
	r.FirstBadChainSeq, r.Checked, r.ChainValid = bad, checked, bad == 0
	if req.GetFull() && r.ChainValid {
		if err := s.recordFullRun(ctx, from, checked, prev); err != nil {
			return nil, internal(err)
		}
	}
	if r.Gaps, err = s.gaps(ctx); err != nil {
		return nil, internal(err)
	}
	if r.StuckOutbox, err = s.stuck(ctx); err != nil {
		return nil, internal(err)
	}
	n, okCP, detail, err := s.verifyCheckpoints(ctx)
	if err != nil {
		return nil, internal(err)
	}
	r.Checkpoints, r.CheckpointsValid, r.Detail = int32(n), okCP, detail
	return r, nil
}

// resumeFromFullRun moves the start of an incremental walk to just after the last recorded full
// run. It returns a non-zero bad chain_seq when that run's anchor row changed or disappeared.
func (s *Service) resumeFromFullRun(ctx context.Context, r *auditv1.VerifyReport, from int64, prev string) (int64, int64, string, error) {
	var through int64
	var hash string
	var ranAt time.Time
	err := s.pool.QueryRow(ctx, `SELECT through_chain_seq, through_hash, ran_at FROM audit_verifications
		ORDER BY through_chain_seq DESC, ran_at DESC LIMIT 1`).Scan(&through, &hash, &ranAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, from, prev, nil // never fully verified: walk everything
	}
	if err != nil {
		return 0, 0, "", err
	}
	r.Incremental, r.FullThroughChainSeq, r.FullVerifiedAt = true, through, timestamppb.New(ranAt)
	if through < from { // the anchor was purged with an archived range
		return 0, from, prev, nil
	}
	var stored string
	err = s.pool.QueryRow(ctx, `SELECT hash FROM audit_events WHERE chain_seq=$1`, through).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && stored != hash) {
		r.Detail = fmt.Sprintf("chain at %d differs from the last full verification", through)
		return through, 0, "", nil
	}
	if err != nil {
		return 0, 0, "", err
	}
	return 0, through + 1, hash, nil
}

// recordFullRun stores the anchor of a valid full walk.
func (s *Service) recordFullRun(ctx context.Context, from, checked int64, prev string) error {
	through, hash := from-1, prev
	if checked > 0 {
		through = from + checked - 1
		if err := s.pool.QueryRow(ctx, `SELECT hash FROM audit_events WHERE chain_seq=$1`, through).Scan(&hash); err != nil {
			return err
		}
	}
	if through < 1 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO audit_verifications (through_chain_seq, through_hash, checked) VALUES ($1, $2, $3)`, through, hash, checked)
	return err
}

// chainStart is where verification begins: chain_seq 1, or right after the last purged range
// (its archive manifest carries the hash to link to).
func (s *Service) chainStart(ctx context.Context) (int64, string, error) {
	var through int64
	var last string
	err := s.pool.QueryRow(ctx, `SELECT coalesce(max(last_chain_seq), 0), coalesce((SELECT last_hash FROM audit_archives WHERE purged_at IS NOT NULL ORDER BY last_chain_seq DESC LIMIT 1), '')
		FROM audit_archives WHERE purged_at IS NOT NULL`).Scan(&through, &last)
	if err != nil {
		if strings.Contains(err.Error(), "audit_archives") { // before the retention migration
			return 1, "", nil
		}
		return 0, "", err
	}
	if through == 0 {
		return 1, "", nil
	}
	return through + 1, last, nil
}

// gaps lists missing source_seq ranges per source. A missing range is only reported once the
// event after it is older than GapGrace (SQS may deliver out of order).
func (s *Service) gaps(ctx context.Context) ([]*auditv1.Gap, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT source, coalesce(prev, 0) + 1, source_seq - 1 FROM (
			SELECT source, source_seq, recorded_at, lag(source_seq) OVER (PARTITION BY source ORDER BY source_seq) AS prev
			FROM (SELECT DISTINCT ON (source, source_seq) source, source_seq, recorded_at FROM audit_events WHERE source IS NOT NULL
			      ORDER BY source, source_seq, recorded_at) d) x
		WHERE source_seq > coalesce(prev, 0) + 1 AND recorded_at < $1
		ORDER BY source, source_seq`, s.now().Add(-s.cfg.GapGrace))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*auditv1.Gap
	for rows.Next() {
		g := &auditv1.Gap{}
		if err := rows.Scan(&g.Source, &g.FromSeq, &g.ToSeq); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// stuck counts producer outbox rows not published within StuckAfter.
func (s *Service) stuck(ctx context.Context) ([]*auditv1.StuckOutbox, error) {
	var out []*auditv1.StuckOutbox
	for _, schema := range s.cfg.ProducerSchemas {
		var exists bool
		if err := s.pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, schema+".outbox").Scan(&exists); err != nil || !exists {
			continue
		}
		var n int64
		var oldest *time.Time
		if err := s.pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*), min(created_at) FROM %s.outbox WHERE published_at IS NULL AND created_at < $1`,
			pgx.Identifier{schema}.Sanitize()), s.now().Add(-s.cfg.StuckAfter)).Scan(&n, &oldest); err != nil {
			return nil, err
		}
		if n > 0 {
			out = append(out, &auditv1.StuckOutbox{Source: schema, Count: n, Oldest: timestamppb.New(*oldest)})
		}
	}
	return out, nil
}

type checkpointDoc struct {
	ChainSeq             int64     `json:"chain_seq"`
	Hash                 string    `json:"hash"`
	CreatedAt            time.Time `json:"created_at"`
	PrevCheckpointSHA256 string    `json:"prev_checkpoint_sha256"`
}

func checkpointKey(at time.Time, seq int64) string {
	return fmt.Sprintf("checkpoints/%s/%020d.json", at.Format("2006-01-02"), seq)
}

// listCheckpoints returns checkpoint keys in chain order (the zero-padded seq orders them).
func (s *Service) listCheckpoints(ctx context.Context) ([]string, error) {
	keys, err := s.objects.List(ctx, "checkpoints/")
	if err != nil {
		return nil, err
	}
	var docs []string
	for _, k := range keys {
		if strings.HasSuffix(k, ".json") {
			docs = append(docs, k)
		}
	}
	// keys sort by date first; order by the seq part instead
	sortBySeq(docs)
	return docs, nil
}

func sortBySeq(keys []string) {
	seq := func(k string) string { return k[strings.LastIndex(k, "/")+1:] }
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && seq(keys[j]) < seq(keys[j-1]); j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
}

// verifyCheckpoints checks each checkpoint's signature, its link to the previous checkpoint and
// that the stored chain still has the anchored hash at the anchored chain_seq.
func (s *Service) verifyCheckpoints(ctx context.Context) (int, bool, string, error) {
	keys, err := s.listCheckpoints(ctx)
	if err != nil {
		return 0, false, "", err
	}
	pub := s.key.Public().(ed25519.PublicKey)
	prevSum := ""
	for _, k := range keys {
		body, err := s.objects.Get(ctx, k)
		if err != nil {
			return len(keys), false, "unreadable " + k, nil
		}
		sigRaw, err := s.objects.Get(ctx, k+".sig")
		if err != nil {
			return len(keys), false, "missing signature for " + k, nil
		}
		sig, err := base64.StdEncoding.DecodeString(string(sigRaw))
		if err != nil || !ed25519.Verify(pub, body, sig) {
			return len(keys), false, "bad signature on " + k, nil
		}
		var doc checkpointDoc
		if err := json.Unmarshal(body, &doc); err != nil {
			return len(keys), false, "malformed " + k, nil
		}
		if doc.PrevCheckpointSHA256 != prevSum {
			return len(keys), false, "broken checkpoint link at " + k, nil
		}
		sum := sha256.Sum256(body)
		prevSum = hex.EncodeToString(sum[:])
		var stored string
		err = s.pool.QueryRow(ctx, `SELECT hash FROM audit_events WHERE chain_seq=$1`, doc.ChainSeq).Scan(&stored)
		if err == pgx.ErrNoRows {
			if ok, err := s.archivedHash(ctx, doc.ChainSeq, doc.Hash); err != nil || !ok {
				return len(keys), false, fmt.Sprintf("anchored chain_seq %d not found", doc.ChainSeq), err
			}
			continue
		}
		if err != nil {
			return len(keys), false, "", err
		}
		if stored != doc.Hash {
			return len(keys), false, fmt.Sprintf("chain at %d differs from checkpoint %s", doc.ChainSeq, k), nil
		}
	}
	return len(keys), true, "", nil
}

// archivedHash checks a purged anchor against its archive manifest.
func (s *Service) archivedHash(ctx context.Context, seq int64, hash string) (bool, error) {
	return s.archivedHashFromManifest(ctx, seq, hash)
}
