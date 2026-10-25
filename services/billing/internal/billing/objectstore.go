package billing

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// ObjectStore keeps statement documents (S3 in every environment, LocalStack locally).
type ObjectStore interface {
	Put(ctx context.Context, key string, body []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
}

// ErrObjectNotFound is returned by Get for a missing key.
var ErrObjectNotFound = errors.New("billing: object not found")

// S3Store writes statements to one bucket.
type S3Store struct {
	client *s3.Client
	bucket string
}

// NewS3Store returns an ObjectStore on bucket.
func NewS3Store(client *s3.Client, bucket string) *S3Store {
	return &S3Store{client: client, bucket: bucket}
}

// Put stores body as application/json under key.
func (s *S3Store) Put(ctx context.Context, key string, body []byte) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key), Body: bytes.NewReader(body),
		ContentType: aws.String("application/json")})
	return err
}

// Get reads the object at key.
func (s *S3Store) Get(ctx context.Context, key string) ([]byte, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()
	return io.ReadAll(out.Body)
}

// MemStore is an in-memory ObjectStore for tests.
type MemStore struct {
	mu   sync.Mutex
	objs map[string][]byte
}

// NewMemStore returns an empty MemStore.
func NewMemStore() *MemStore { return &MemStore{objs: map[string][]byte{}} }

// Put stores a copy of body.
func (m *MemStore) Put(_ context.Context, key string, body []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[key] = append([]byte(nil), body...)
	return nil
}

// Get returns a copy of the object.
func (m *MemStore) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[key]
	if !ok {
		return nil, ErrObjectNotFound
	}
	return append([]byte(nil), b...), nil
}
