// Package awsx builds AWS SDK v2 clients. A non-empty endpoint targets LocalStack.
package awsx

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// Clients bundles the AWS clients the services use.
type Clients struct {
	SNS *sns.Client
	SQS *sqs.Client
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
	return &Clients{SNS: sns.NewFromConfig(cfg), SQS: sqs.NewFromConfig(cfg)}, nil
}
