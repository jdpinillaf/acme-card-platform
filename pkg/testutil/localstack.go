//go:build integration

package testutil

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/localstack"

	"github.com/acme/acme-card/pkg/awsx"
)

// AWSFixture is a running LocalStack with one SNS topic fanning out to one SQS queue.
type AWSFixture struct {
	Endpoint string
	TopicARN string
	QueueURL string
	SNS      *sns.Client
	SQS      *sqs.Client
}

// StartLocalStack launches LocalStack (sns, sqs) and wires topic → queue with raw delivery.
func StartLocalStack(t testing.TB) *AWSFixture {
	t.Helper()
	ctx := context.Background()
	c, err := localstack.Run(ctx, "localstack/localstack:3.8",
		testcontainers.WithEnv(map[string]string{"SERVICES": "sns,sqs", "SQS_ENDPOINT_STRATEGY": "path"}),
	)
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
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	topic, err := clients.SNS.CreateTopic(ctx, &sns.CreateTopicInput{Name: aws.String("acme-card-events")})
	if err != nil {
		t.Fatal(err)
	}
	q, err := clients.SQS.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String("acme-card-audit")})
	if err != nil {
		t.Fatal(err)
	}
	attrs, err := clients.SQS.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: q.QueueUrl, AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameQueueArn}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = clients.SNS.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topic.TopicArn, Protocol: aws.String("sqs"), Endpoint: aws.String(attrs.Attributes["QueueArn"]),
		Attributes: map[string]string{"RawMessageDelivery": "true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &AWSFixture{Endpoint: endpoint, TopicARN: *topic.TopicArn, QueueURL: *q.QueueUrl, SNS: clients.SNS, SQS: clients.SQS}
}
