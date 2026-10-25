package audit

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	auditv1 "github.com/acme/acme-card/gen/go/acme/audit/v1"
)

// CreateCheckpoint anchors the current chain head outside the database: a signed JSON document,
// linked to the previous checkpoint, written once to the Object-Lock bucket (spec I-3).
func (s *Service) CreateCheckpoint(ctx context.Context, _ *auditv1.CreateCheckpointRequest) (*auditv1.Checkpoint, error) {
	seq, hash, err := s.store.Head(ctx)
	if err != nil {
		return nil, internal(err)
	}
	keys, err := s.listCheckpoints(ctx)
	if err != nil {
		return nil, internal(err)
	}
	prevSum := ""
	if len(keys) > 0 {
		last, err := s.objects.Get(ctx, keys[len(keys)-1])
		if err != nil {
			return nil, internal(err)
		}
		var lastDoc checkpointDoc
		if err := json.Unmarshal(last, &lastDoc); err == nil && lastDoc.ChainSeq == seq {
			return checkpointProto(lastDoc, keys[len(keys)-1], ""), nil // nothing new since the last anchor
		}
		sum := sha256.Sum256(last)
		prevSum = hex.EncodeToString(sum[:])
	}
	now := s.now()
	doc := checkpointDoc{ChainSeq: seq, Hash: hash, CreatedAt: now, PrevCheckpointSHA256: prevSum}
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, internal(err)
	}
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(s.key, body))
	key := checkpointKey(now, seq)
	until := now.Add(s.cfg.ArchiveLock)
	if err := s.objects.PutLocked(ctx, key, body, until); err != nil {
		return nil, internal(err)
	}
	if err := s.objects.PutLocked(ctx, key+".sig", []byte(sig), until); err != nil {
		return nil, internal(err)
	}
	return checkpointProto(doc, key, sig), nil
}

func checkpointProto(d checkpointDoc, key, sig string) *auditv1.Checkpoint {
	return &auditv1.Checkpoint{ChainSeq: d.ChainSeq, Hash: d.Hash, CreatedAt: timestamppb.New(d.CreatedAt), PrevCheckpointSha256: d.PrevCheckpointSHA256,
		S3Key: key, Signature: sig}
}

func internal(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, err.Error())
	}
	return status.Error(codes.Internal, err.Error())
}

// archivedHashFromManifest: placeholder until retention (Task 7) archives purged ranges.
func (s *Service) archivedHashFromManifest(context.Context, int64, string) (bool, error) {
	return false, nil
}
