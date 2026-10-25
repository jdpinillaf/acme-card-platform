//go:build integration

package audit

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/localstack"

	"github.com/acme/acme-card/pkg/awsx"
)

func TestS3StoreWritesObjectLockRetention(t *testing.T) {
	ctx := context.Background()
	c, err := localstack.Run(ctx, "localstack/localstack:3.8", testcontainers.WithEnv(map[string]string{"SERVICES": "s3"}))
	if err != nil {
		t.Fatalf("start localstack: %v", err)
	}
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	endpoint, err := c.PortEndpoint(ctx, "4566/tcp", "http")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	clients, err := awsx.New(ctx, "us-east-1", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clients.S3.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("audit-test"), ObjectLockEnabledForBucket: aws.Bool(true)}); err != nil {
		t.Fatal(err)
	}
	st := NewS3Store(clients.S3, "audit-test")
	until := time.Now().Add(7 * 365 * 24 * time.Hour).UTC().Truncate(time.Second)
	if err := st.PutLocked(ctx, "checkpoints/x/1.json", []byte(`{"a":1}`), until); err != nil {
		t.Fatal(err)
	}
	head, err := clients.S3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("audit-test"), Key: aws.String("checkpoints/x/1.json")})
	if err != nil {
		t.Fatal(err)
	}
	if string(head.ObjectLockMode) != "COMPLIANCE" || head.ObjectLockRetainUntilDate == nil || !head.ObjectLockRetainUntilDate.Equal(until) {
		t.Fatalf("retention: mode=%s until=%v", head.ObjectLockMode, head.ObjectLockRetainUntilDate)
	}
	got, err := st.Get(ctx, "checkpoints/x/1.json")
	if err != nil || !bytes.Equal(got, []byte(`{"a":1}`)) {
		t.Fatalf("get: %q %v", got, err)
	}
	keys, err := st.List(ctx, "checkpoints/")
	if err != nil || len(keys) != 1 {
		t.Fatalf("list: %v %v", keys, err)
	}
	if _, err := st.Get(ctx, "missing"); err != ErrNotFound {
		t.Fatalf("missing: %v", err)
	}
}
