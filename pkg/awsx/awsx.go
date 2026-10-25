// Package awsx builds AWS SDK v2 clients. A non-empty endpoint targets LocalStack.
package awsx

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// Clients bundles the AWS clients the services use.
type Clients struct {
	SNS *sns.Client
	SQS *sqs.Client
	S3  *s3.Client
}

// New loads default credentials and, when endpoint != "", points every client at it.
func New(ctx context.Context, region, endpoint string) (*Clients, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("awsx: load config: %w", err)
	}
	if endpoint != "" {
		cfg.BaseEndpoint = aws.String(endpoint)
	}
	// LocalStack serves S3 path-style (no bucket subdomains).
	s3c := s3.NewFromConfig(cfg, func(o *s3.Options) { o.UsePathStyle = endpoint != "" })
	return &Clients{SNS: sns.NewFromConfig(cfg), SQS: sqs.NewFromConfig(cfg), S3: s3c}, nil
}
