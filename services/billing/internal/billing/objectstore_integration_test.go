//go:build integration

package billing

import (
	"bytes"
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/localstack"

	"github.com/acme/acme-card/pkg/awsx"
)

func TestS3StoreRoundTrip(t *testing.T) {
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
	if _, err := clients.S3.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("acme-card-statements-test")}); err != nil {
		t.Fatal(err)
	}
	st := NewS3Store(clients.S3, "acme-card-statements-test")
	body := []byte(`{"cycle":1}`)
	if err := st.Put(ctx, "statements/a/1.json", body); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(ctx, "statements/a/1.json")
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("get: %q %v", got, err)
	}
	if _, err := st.Get(ctx, "statements/a/missing.json"); err == nil {
		t.Fatal("missing key must error")
	}
}
