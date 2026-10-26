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
// retention (in chunks), verify the archives by reading them back, then purge what verified
// archives cover. Each step takes the retention lock, so auditctl and the job never interleave.
func (s *Service) RunRetention(ctx context.Context, req *auditv1.RunRetentionRequest) (*auditv1.RetentionReport, error) {
	now := s.now()
	if req.GetNow() != nil {
		now = req.GetNow().AsTime()
	}
	rep := &auditv1.RetentionReport{}
	for {
		n, key, err := s.Archive(ctx, now)
		if err != nil {
			return nil, internal(err)
		}
		if n == 0 {
			break
		}
		rep.Archived, rep.ManifestKey = rep.Archived+n, key
	}
	if err := s.VerifyArchives(ctx); err != nil {
		return nil, internal(err)
	}
	var err error
	if rep.Purged, err = s.Purge(ctx); err != nil {
		return nil, internal(err)
	}
	return rep, nil
}

// withLock runs fn holding a session advisory lock named name (shared by every replica and auditctl).
func (s *Service) withLock(ctx context.Context, name string, fn func() error) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtext($1))`, name); err != nil {
		return err
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtext($1))`, name) //nolint:errcheck
	return fn()
}

// Archive writes the next not-yet-archived chain prefix recorded before the retention cutoff (at
// most ArchiveChunk rows) as NDJSON plus a signed manifest, both Object-Locked, and records it in
// audit_archives (unverified). It returns 0 when nothing is old enough.
func (s *Service) Archive(ctx context.Context, now time.Time) (archived int64, manifestKey string, err error) {
	err = s.withLock(ctx, "audit.retention", func() error {
		archived, manifestKey, err = s.archiveLocked(ctx, now)
		return err
	})
	return archived, manifestKey, err
}

func (s *Service) archiveLocked(ctx context.Context, now time.Time) (int64, string, error) {
	var first, through int64
	if err := s.pool.QueryRow(ctx, `SELECT coalesce(max(last_chain_seq), 0) + 1 FROM audit_archives`).Scan(&first); err != nil {
		return 0, "", err
	}
	// the archive is a contiguous chain prefix: it stops before the first row young enough to keep
	if err := s.pool.QueryRow(ctx, `SELECT coalesce(
			(SELECT min(chain_seq) FROM audit_events WHERE chain_seq >= $1 AND recorded_at >= $2) - 1,
			(SELECT chain_seq FROM chain_head WHERE id = 1))`, first, s.cfg.cutoff(now)).Scan(&through); err != nil {
		return 0, "", err
	}
	through = min(through, first+s.cfg.ArchiveChunk-1)
	if through < first {
		return 0, "", nil
	}
	var buf bytes.Buffer
	m := manifestDoc{FirstChainSeq: first, LastChainSeq: through, CreatedAt: s.now()}
	floors := map[string]int64{}
	expect := first
	err := s.store.Each(ctx, first, through, func(ev events.Event, seq int64, prev, hash string) error {
		if seq != expect {
			return fmt.Errorf("audit: chain_seq %d missing, cannot archive", expect)
		}
		if seq == first {
			m.FirstPrevHash = prev
		}
		if ev.Source != "" && ev.SourceSeq > floors[ev.Source] {
			floors[ev.Source] = ev.SourceSeq
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
	floorsJSON, err := json.Marshal(floors)
	if err != nil {
		return 0, "", err
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO audit_archives (first_chain_seq, last_chain_seq, first_prev_hash, last_hash, count, sha256, s3_key, manifest_key, source_floors)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, m.FirstChainSeq, m.LastChainSeq, m.FirstPrevHash, m.LastHash, m.Count, m.SHA256, m.ArchiveKey, mKey, floorsJSON); err != nil {
		return 0, "", err
	}
	return m.Count, mKey, nil
}

// loadManifest reads a manifest and checks its signature (errArchiveInvalid on any mismatch).
func (s *Service) loadManifest(ctx context.Context, manifestKey string) (manifestDoc, error) {
	var m manifestDoc
	manifest, err := s.objects.Get(ctx, manifestKey)
	if errors.Is(err, ErrNotFound) {
		return m, fmt.Errorf("%w: missing manifest %s", errArchiveInvalid, manifestKey)
	}
	if err != nil {
		return m, err
	}
	sigRaw, err := s.objects.Get(ctx, manifestKey+".sig")
	if err != nil && !errors.Is(err, ErrNotFound) {
		return m, err
	}
	sig, _ := base64.StdEncoding.DecodeString(string(sigRaw))
	if !ed25519.Verify(s.key.Public().(ed25519.PublicKey), manifest, sig) {
		return m, fmt.Errorf("%w: bad manifest signature %s", errArchiveInvalid, manifestKey)
	}
	if err := json.Unmarshal(manifest, &m); err != nil || m.LastChainSeq < m.FirstChainSeq || m.Count != m.LastChainSeq-m.FirstChainSeq+1 {
		return m, fmt.Errorf("%w: malformed manifest %s", errArchiveInvalid, manifestKey)
	}
	return m, nil
}

// archiveHashes is a verified archive: its manifest and the hash of every line, in chain order.
type archiveHashes struct {
	m      manifestDoc
	hashes []string
}

// loadArchive reads an archive back: manifest signature, body checksum, and a re-hash of every
// line from the manifest's first prev hash. It returns errArchiveInvalid on any mismatch.
func (s *Service) loadArchive(ctx context.Context, manifestKey string) (archiveHashes, error) {
	m, err := s.loadManifest(ctx, manifestKey)
	if err != nil {
		return archiveHashes{}, err
	}
	body, err := s.objects.Get(ctx, m.ArchiveKey)
	if errors.Is(err, ErrNotFound) {
		return archiveHashes{}, fmt.Errorf("%w: missing archive %s", errArchiveInvalid, m.ArchiveKey)
	}
	if err != nil {
		return archiveHashes{}, err
	}
	if sum := sha256.Sum256(body); hex.EncodeToString(sum[:]) != m.SHA256 {
		return archiveHashes{}, fmt.Errorf("%w: archive %s does not match its manifest", errArchiveInvalid, m.ArchiveKey)
	}
	hashes := make([]string, 0, m.Count)
	prev, expect := m.FirstPrevHash, m.FirstChainSeq
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var l archiveLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			return archiveHashes{}, fmt.Errorf("%w: malformed line", errArchiveInvalid)
		}
		canon, err := store.Canonical(l.Event, l.ChainSeq)
		if err != nil || l.ChainSeq != expect || l.PrevHash != prev || store.Hash(prev, canon) != l.Hash {
			return archiveHashes{}, fmt.Errorf("%w: chain broken at %d in %s", errArchiveInvalid, expect, m.ArchiveKey)
		}
		hashes = append(hashes, l.Hash)
		prev, expect = l.Hash, expect+1
	}
	if err := sc.Err(); err != nil {
		return archiveHashes{}, err
	}
	if expect != m.LastChainSeq+1 || prev != m.LastHash || int64(len(hashes)) != m.Count {
		return archiveHashes{}, fmt.Errorf("%w: archive %s is incomplete", errArchiveInvalid, m.ArchiveKey)
	}
	return archiveHashes{m: m, hashes: hashes}, nil
}

// VerifyArchives checks every unverified archive against its manifest, the database row and the
// chain around it, and marks it verified. Purge only trusts verified archives.
func (s *Service) VerifyArchives(ctx context.Context) error {
	return s.withLock(ctx, "audit.retention", func() error { return s.verifyArchivesLocked(ctx) })
}

func (s *Service) verifyArchivesLocked(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `SELECT id::text, first_chain_seq, last_chain_seq, first_prev_hash, last_hash, manifest_key,
			coalesce(lag(last_hash) OVER (ORDER BY last_chain_seq), $1), coalesce(lag(last_chain_seq) OVER (ORDER BY last_chain_seq), 0), verified_at IS NULL
		FROM audit_archives ORDER BY last_chain_seq`, store.GenesisHash)
	if err != nil {
		return err
	}
	type pending struct {
		id, firstPrev, lastHash, manifest, linkPrev string
		first, last, prevLast                       int64
	}
	var todo []pending
	for rows.Next() {
		var p pending
		var unverified bool
		if err := rows.Scan(&p.id, &p.first, &p.last, &p.firstPrev, &p.lastHash, &p.manifest, &p.linkPrev, &p.prevLast, &unverified); err != nil {
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
		a, err := s.loadArchive(ctx, p.manifest)
		if err != nil {
			return err
		}
		m := a.m
		if m.FirstChainSeq != p.first || m.LastChainSeq != p.last || m.FirstPrevHash != p.firstPrev || m.LastHash != p.lastHash ||
			p.firstPrev != p.linkPrev || p.first != p.prevLast+1 {
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
// append-only trigger allows) and returns how many were removed. Every archive it relies on must
// still have a valid signed manifest matching its record.
func (s *Service) Purge(ctx context.Context) (n int64, err error) {
	err = s.withLock(ctx, "audit.retention", func() error {
		rows, err := s.pool.Query(ctx, `SELECT first_chain_seq, last_chain_seq, last_hash, manifest_key FROM audit_archives
			WHERE verified_at IS NOT NULL AND purged_at IS NULL ORDER BY last_chain_seq`)
		if err != nil {
			return err
		}
		type arch struct {
			first, last int64
			hash, key   string
		}
		var todo []arch
		for rows.Next() {
			var a arch
			if err := rows.Scan(&a.first, &a.last, &a.hash, &a.key); err != nil {
				rows.Close()
				return err
			}
			todo = append(todo, a)
		}
		rows.Close()
		if err := rows.Err(); err != nil || len(todo) == 0 {
			return err
		}
		for _, a := range todo {
			m, err := s.loadManifest(ctx, a.key)
			if err != nil {
				return err
			}
			if m.FirstChainSeq != a.first || m.LastChainSeq != a.last || m.LastHash != a.hash {
				return fmt.Errorf("%w: archive %s disagrees with its record", errArchiveInvalid, a.key)
			}
		}
		return s.pool.QueryRow(ctx, `SELECT audit_purge_archived($1)`, todo[len(todo)-1].last).Scan(&n)
	})
	return n, err
}

// archivedHashFromManifest checks a purged checkpoint anchor against the archive that holds it;
// cache keeps archives read during one verification.
func (s *Service) archivedHashFromManifest(ctx context.Context, seq int64, hash string, cache map[string]archiveHashes) (bool, error) {
	var first, last int64
	var manifest string
	err := s.pool.QueryRow(ctx, `SELECT first_chain_seq, last_chain_seq, manifest_key FROM audit_archives WHERE $1 BETWEEN first_chain_seq AND last_chain_seq`, seq).
		Scan(&first, &last, &manifest)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	a, ok := cache[manifest]
	if !ok {
		if a, err = s.loadArchive(ctx, manifest); errors.Is(err, errArchiveInvalid) {
			return false, nil
		} else if err != nil {
			return false, err
		}
		cache[manifest] = a
	}
	if a.m.FirstChainSeq != first || a.m.LastChainSeq != last || seq < a.m.FirstChainSeq || seq > a.m.LastChainSeq {
		return false, nil // the database row disagrees with the signed manifest
	}
	return a.hashes[seq-a.m.FirstChainSeq] == hash, nil
}
