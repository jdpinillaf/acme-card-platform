package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// ObjectStore keeps checkpoints, reconciliation reports, exports and archives. Locked objects are
// written once with an S3 Object Lock retention (COMPLIANCE): nobody can delete or overwrite them
// before the date.
type ObjectStore interface {
	Put(ctx context.Context, key string, body []byte) error
	PutLocked(ctx context.Context, key string, body []byte, retainUntil time.Time) error
	Get(ctx context.Context, key string) ([]byte, error)
	List(ctx context.Context, prefix string) ([]string, error)
}

// ErrNotFound is returned by Get for a missing key.
var ErrNotFound = errors.New("audit: object not found")

// S3Store is the AWS / LocalStack implementation.
type S3Store struct {
	client *s3.Client
	bucket string
}

// NewS3Store returns an ObjectStore on bucket (created with Object Lock enabled).
func NewS3Store(client *s3.Client, bucket string) *S3Store {
	return &S3Store{client: client, bucket: bucket}
}

func (s *S3Store) put(ctx context.Context, key string, body []byte, until *time.Time) error {
	sum := sha256.Sum256(body)
	in := &s3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key), Body: bytes.NewReader(body),
		ContentType: aws.String("application/json"), ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(sum[:]))}
	if until != nil {
		in.ObjectLockMode = s3types.ObjectLockModeCompliance
		in.ObjectLockRetainUntilDate = until
	}
	_, err := s.client.PutObject(ctx, in)
	return err
}

// Put stores an ordinary object.
func (s *S3Store) Put(ctx context.Context, key string, body []byte) error {
	return s.put(ctx, key, body, nil)
}

// PutLocked stores a write-once object retained until retainUntil.
func (s *S3Store) PutLocked(ctx context.Context, key string, body []byte, retainUntil time.Time) error {
	return s.put(ctx, key, body, &retainUntil)
}

// Get reads an object.
func (s *S3Store) Get(ctx context.Context, key string) ([]byte, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		var nsk *s3types.NoSuchKey
		if errors.As(err, &nsk) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	defer out.Body.Close()
	return io.ReadAll(out.Body)
}

// List returns the keys under prefix, sorted.
func (s *S3Store) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{Bucket: aws.String(s.bucket), Prefix: aws.String(prefix)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, o := range page.Contents {
			keys = append(keys, aws.ToString(o.Key))
		}
	}
	sort.Strings(keys)
	return keys, nil
}

// MemStore is an in-memory ObjectStore for tests; it refuses to overwrite locked objects.
type MemStore struct {
	mu     sync.Mutex
	objs   map[string][]byte
	locked map[string]time.Time
}

// NewMemStore returns an empty MemStore.
func NewMemStore() *MemStore {
	return &MemStore{objs: map[string][]byte{}, locked: map[string]time.Time{}}
}

// Put stores a copy.
func (m *MemStore) Put(_ context.Context, key string, body []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.locked[key]; ok {
		return errors.New("object is locked")
	}
	m.objs[key] = append([]byte(nil), body...)
	return nil
}

// PutLocked stores a write-once copy.
func (m *MemStore) PutLocked(ctx context.Context, key string, body []byte, until time.Time) error {
	if err := m.Put(ctx, key, body); err != nil {
		return err
	}
	m.mu.Lock()
	m.locked[key] = until
	m.mu.Unlock()
	return nil
}

// Get returns a copy.
func (m *MemStore) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[key]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), b...), nil
}

// List returns sorted keys under prefix.
func (m *MemStore) List(_ context.Context, prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var keys []string
	for k := range m.objs {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

// Locked reports whether key was written with a retention (test helper).
func (m *MemStore) Locked(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.locked[key]
	return ok
}

// Overwrite replaces an object ignoring locks (test helper simulating a forged upload).
func (m *MemStore) Overwrite(key string, body []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[key] = append([]byte(nil), body...)
	return nil
}
