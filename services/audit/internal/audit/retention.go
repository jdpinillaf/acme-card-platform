package audit

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	auditv1 "github.com/acme/acme-card/gen/go/acme/audit/v1"
	"github.com/acme/acme-card/pkg/events"
	"github.com/acme/acme-card/services/audit/internal/store"
)

// archiveLine is one archived row: the event as hashed plus its chain links.
type archiveLine struct {
	ChainSeq int64        `json:"chain_seq"`
	PrevHash string       `json:"prev_hash"`
	Hash     string       `json:"hash"`
	Event    events.Event `json:"event"`
}

// manifestDoc describes one archive; it is signed like a checkpoint.
type manifestDoc struct {
	FirstChainSeq int64     `json:"first_chain_seq"`
	LastChainSeq  int64     `json:"last_chain_seq"`
	FirstPrevHash string    `json:"first_prev_hash"`
	LastHash      string    `json:"last_hash"`
	Count         int64     `json:"count"`
	SHA256        string    `json:"sha256"`
	ArchiveKey    string    `json:"archive_key"`
	CreatedAt     time.Time `json:"created_at"`
}

// errArchiveInvalid marks an archive that fails its integrity checks (not an infrastructure error).
var errArchiveInvalid = errors.New("audit: archive integrity check failed")

// RunRetention is the monthly job (spec I-7, B-5): archive every row older than the hot
// retention, verify the archive by reading it back, then purge what verified archives cover.
func (s *Service) RunRetention(ctx context.Context, req *auditv1.RunRetentionRequest) (*auditv1.RetentionReport, error) {
	now := s.now()
	if req.GetNow() != nil {
		now = req.GetNow().AsTime()
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, internal(err)
	}
	defer conn.Release()
	// one retention run at a time (several audit replicas share the schedule)
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtext('audit.retention'))`); err != nil {
		return nil, internal(err)
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtext('audit.retention'))`) //nolint:errcheck
	rep := &auditv1.RetentionReport{}
	if rep.Archived, rep.ManifestKey, err = s.Archive(ctx, now); err != nil {
		return nil, internal(err)
	}
	if err := s.VerifyArchives(ctx); err != nil {
		return nil, internal(err)
	}
	if rep.Purged, err = s.Purge(ctx); err != nil {
		return nil, internal(err)
	}
	return rep, nil
}

// Archive writes the not-yet-archived chain prefix recorded before now-Retention as NDJSON plus a
// signed manifest, both Object-Locked, and records it in audit_archives (unverified).
func (s *Service) Archive(ctx context.Context, now time.Time) (int64, string, error) {
	var first, through int64
	if err := s.pool.QueryRow(ctx, `SELECT coalesce(max(last_chain_seq), 0) + 1 FROM audit_archives`).Scan(&first); err != nil {
		return 0, "", err
	}
	// the archive is a contiguous chain prefix: it stops before the first row young enough to keep
	if err := s.pool.QueryRow(ctx, `SELECT coalesce(
			(SELECT min(chain_seq) FROM audit_events WHERE chain_seq >= $1 AND recorded_at >= $2) - 1,
			(SELECT chain_seq FROM chain_head WHERE id = 1))`, first, now.Add(-s.cfg.Retention)).Scan(&through); err != nil {
		return 0, "", err
	}
	if through < first {
		return 0, "", nil
	}
	var buf bytes.Buffer
	m := manifestDoc{FirstChainSeq: first, LastChainSeq: through, CreatedAt: s.now()}
	expect := first
	err := s.store.Each(ctx, first, through, func(ev events.Event, seq int64, prev, hash string) error {
		if seq != expect {
			return fmt.Errorf("audit: chain_seq %d missing, cannot archive", expect)
		}
		if seq == first {
			m.FirstPrevHash = prev
		}
		line, err := json.Marshal(archiveLine{ChainSeq: seq, PrevHash: prev, Hash: hash, Event: ev})
		if err != nil {
			return err
		}
		buf.Write(line)
		buf.WriteByte('\n')
		m.LastHash = hash
		expect++
		return nil
	})
	if err != nil {
		return 0, "", err
	}
	if expect != through+1 {
		return 0, "", fmt.Errorf("audit: chain_seq %d missing, cannot archive", expect)
	}
	m.Count = through - first + 1
	sum := sha256.Sum256(buf.Bytes())
	m.SHA256 = hex.EncodeToString(sum[:])
	base := fmt.Sprintf("archives/%020d-%020d", first, through)
	m.ArchiveKey = base + ".ndjson"
	manifest, err := json.Marshal(m)
	if err != nil {
		return 0, "", err
	}
	until := s.now().Add(s.cfg.ArchiveLock)
	mKey := base + ".manifest.json"
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(s.key, manifest))
	for _, o := range []struct {
		key  string
		body []byte
	}{{m.ArchiveKey, buf.Bytes()}, {mKey, manifest}, {mKey + ".sig", []byte(sig)}} {
		if err := s.objects.PutLocked(ctx, o.key, o.body, until); err != nil {
			return 0, "", err
		}
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO audit_archives (first_chain_seq, last_chain_seq, first_prev_hash, last_hash, count, sha256, s3_key, manifest_key)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, m.FirstChainSeq, m.LastChainSeq, m.FirstPrevHash, m.LastHash, m.Count, m.SHA256, m.ArchiveKey, mKey); err != nil {
		return 0, "", err
	}
	return m.Count, mKey, nil
}

// loadArchive reads an archive back: manifest signature, body checksum, and a re-hash of every
// line from the manifest's first prev hash. It returns errArchiveInvalid on any mismatch.
func (s *Service) loadArchive(ctx context.Context, manifestKey string) (manifestDoc, []archiveLine, error) {
	var m manifestDoc
	manifest, err := s.objects.Get(ctx, manifestKey)
	if errors.Is(err, ErrNotFound) {
		return m, nil, fmt.Errorf("%w: missing manifest %s", errArchiveInvalid, manifestKey)
	}
	if err != nil {
		return m, nil, err
	}
	sigRaw, err := s.objects.Get(ctx, manifestKey+".sig")
	if err != nil && !errors.Is(err, ErrNotFound) {
		return m, nil, err
	}
	sig, _ := base64.StdEncoding.DecodeString(string(sigRaw))
	if !ed25519.Verify(s.key.Public().(ed25519.PublicKey), manifest, sig) {
		return m, nil, fmt.Errorf("%w: bad manifest signature %s", errArchiveInvalid, manifestKey)
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		return m, nil, fmt.Errorf("%w: malformed manifest", errArchiveInvalid)
	}
	body, err := s.objects.Get(ctx, m.ArchiveKey)
	if errors.Is(err, ErrNotFound) {
		return m, nil, fmt.Errorf("%w: missing archive %s", errArchiveInvalid, m.ArchiveKey)
	}
	if err != nil {
		return m, nil, err
	}
	if sum := sha256.Sum256(body); hex.EncodeToString(sum[:]) != m.SHA256 {
		return m, nil, fmt.Errorf("%w: archive %s does not match its manifest", errArchiveInvalid, m.ArchiveKey)
	}
	var lines []archiveLine
	prev, expect := m.FirstPrevHash, m.FirstChainSeq
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var l archiveLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			return m, nil, fmt.Errorf("%w: malformed line", errArchiveInvalid)
		}
		canon, err := store.Canonical(l.Event, l.ChainSeq)
		if err != nil || l.ChainSeq != expect || l.PrevHash != prev || store.Hash(prev, canon) != l.Hash {
			return m, nil, fmt.Errorf("%w: chain broken at %d in %s", errArchiveInvalid, expect, m.ArchiveKey)
		}
		lines = append(lines, l)
		prev, expect = l.Hash, expect+1
	}
	if err := sc.Err(); err != nil {
		return m, nil, err
	}
	if expect != m.LastChainSeq+1 || prev != m.LastHash || int64(len(lines)) != m.Count {
		return m, nil, fmt.Errorf("%w: archive %s is incomplete", errArchiveInvalid, m.ArchiveKey)
	}
	return m, lines, nil
}

// VerifyArchives checks every unverified archive against its manifest, the database row and the
// chain around it, and marks it verified. Purge only trusts verified archives.
func (s *Service) VerifyArchives(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `SELECT id::text, first_chain_seq, last_chain_seq, first_prev_hash, last_hash, manifest_key,
			coalesce(lag(last_hash) OVER (ORDER BY last_chain_seq), $1), verified_at IS NULL
		FROM audit_archives ORDER BY last_chain_seq`, store.GenesisHash)
	if err != nil {
		return err
	}
	type pending struct {
		id, firstPrev, lastHash, manifest, linkPrev string
		first, last                                 int64
	}
	var todo []pending
	for rows.Next() {
		var p pending
		var unverified bool
		if err := rows.Scan(&p.id, &p.first, &p.last, &p.firstPrev, &p.lastHash, &p.manifest, &p.linkPrev, &unverified); err != nil {
			rows.Close()
			return err
		}
		if unverified {
			todo = append(todo, p)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range todo {
		m, _, err := s.loadArchive(ctx, p.manifest)
		if err != nil {
			return err
		}
		if m.FirstChainSeq != p.first || m.LastChainSeq != p.last || m.FirstPrevHash != p.firstPrev || m.LastHash != p.lastHash || p.firstPrev != p.linkPrev {
			return fmt.Errorf("%w: archive %s disagrees with its record", errArchiveInvalid, p.manifest)
		}
		// the rows are still in the database: they must be exactly what was archived
		var stored string
		err = s.pool.QueryRow(ctx, `SELECT hash FROM audit_events WHERE chain_seq=$1`, p.last).Scan(&stored)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil && stored != p.lastHash {
			return fmt.Errorf("%w: chain at %d differs from archive %s", errArchiveInvalid, p.last, p.manifest)
		}
		if bad, _, err := s.store.VerifyFrom(ctx, p.first, p.last, p.firstPrev); err != nil || bad != 0 {
			if err == nil {
				err = fmt.Errorf("%w: stored chain broken at %d", errArchiveInvalid, bad)
			}
			return err
		}
		if _, err := s.pool.Exec(ctx, `UPDATE audit_archives SET verified_at = now() WHERE id=$1`, p.id); err != nil {
			return err
		}
	}
	return nil
}

// Purge removes the rows verified archives cover (through audit_purge_archived, the only path the
// append-only trigger allows) and returns how many were removed.
func (s *Service) Purge(ctx context.Context) (int64, error) {
	var through int64
	if err := s.pool.QueryRow(ctx, `SELECT coalesce(max(last_chain_seq), 0) FROM audit_archives
		WHERE verified_at IS NOT NULL AND purged_at IS NULL`).Scan(&through); err != nil || through == 0 {
		return 0, err
	}
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT audit_purge_archived($1)`, through).Scan(&n)
	return n, err
}

// archivedHashFromManifest checks a purged checkpoint anchor against the archive that holds it.
func (s *Service) archivedHashFromManifest(ctx context.Context, seq int64, hash string) (bool, error) {
	var manifest string
	err := s.pool.QueryRow(ctx, `SELECT manifest_key FROM audit_archives WHERE $1 BETWEEN first_chain_seq AND last_chain_seq`, seq).Scan(&manifest)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	m, lines, err := s.loadArchive(ctx, manifest)
	if errors.Is(err, errArchiveInvalid) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return lines[seq-m.FirstChainSeq].Hash == hash, nil
}
