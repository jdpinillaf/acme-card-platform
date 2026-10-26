package notify

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/acme/acme-card/pkg/events"
)

// Consumer long-polls the notifications queue. A message is deleted only after Process succeeds,
// so failures are retried by SQS and, after 5 receives, dead-lettered.
type Consumer struct {
	sqs      *sqs.Client
	queueURL string
	svc      *Service
	log      *slog.Logger
}

// NewConsumer builds a consumer. log may be nil.
func NewConsumer(client *sqs.Client, queueURL string, svc *Service, log *slog.Logger) *Consumer {
	if log == nil {
		log = slog.Default()
	}
	return &Consumer{sqs: client, queueURL: queueURL, svc: svc, log: log}
}

// PollOnce receives up to 10 messages (waiting up to 1s) and returns how many were processed.
func (c *Consumer) PollOnce(ctx context.Context) (int, error) {
	out, err := c.sqs.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(c.queueURL), MaxNumberOfMessages: 10, WaitTimeSeconds: 1})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, m := range out.Messages {
		var ev events.Event
		if err := json.Unmarshal([]byte(aws.ToString(m.Body)), &ev); err != nil || ev.ID == "" {
			c.log.Error("notifications: undecodable message left for DLQ", "message_id", aws.ToString(m.MessageId), "err", err)
			continue
		}
		if err := c.svc.Process(ctx, ev); err != nil {
			c.log.Error("notifications: process failed, will be redelivered", "event_id", ev.ID, "err", err)
			continue
		}
		if _, err := c.sqs.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(c.queueURL), ReceiptHandle: m.ReceiptHandle}); err != nil {
			c.log.Warn("notifications: delete failed, will be redelivered (dedup by id)", "event_id", ev.ID, "err", err)
		}
		n++
	}
	return n, nil
}

// Run polls until ctx is cancelled.
func (c *Consumer) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if _, err := c.PollOnce(ctx); err != nil && ctx.Err() == nil {
			c.log.Error("notifications: poll failed", "err", err)
			time.Sleep(time.Second)
		}
	}
}
