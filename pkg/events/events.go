// Package events defines the domain event envelope and its SNS publisher.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	snstypes "github.com/aws/aws-sdk-go-v2/service/sns/types"
	"github.com/google/uuid"
)

// Event is the envelope written to the outbox, published to SNS and stored by audit.
type Event struct {
	ID                 string          `json:"id"`
	Type               string          `json:"type"`
	AggregateType      string          `json:"aggregate_type"`
	AggregateID        string          `json:"aggregate_id"`
	Actor              string          `json:"actor,omitempty"`
	IdempotencyKeyHash string          `json:"idempotency_key_hash,omitempty"`
	HTTPStatus         int             `json:"http_status,omitempty"`
	Before             json.RawMessage `json:"before,omitempty"`
	After              json.RawMessage `json:"after,omitempty"`
	OccurredAt         time.Time       `json:"occurred_at"`
}

// New builds an event with a fresh UUIDv7 and the current UTC time.
func New(typ, aggregateType, aggregateID string) Event {
	return Event{
		ID:            uuid.Must(uuid.NewV7()).String(),
		Type:          typ,
		AggregateType: aggregateType,
		AggregateID:   aggregateID,
		OccurredAt:    time.Now().UTC(),
	}
}

// Publisher ships events to the bus.
type Publisher interface {
	Publish(ctx context.Context, evs []Event) error
}

// SNSPublisher publishes each event as one SNS message.
type SNSPublisher struct {
	client   *sns.Client
	topicARN string
}

// NewSNSPublisher builds a publisher for topicARN.
func NewSNSPublisher(client *sns.Client, topicARN string) *SNSPublisher {
	return &SNSPublisher{client: client, topicARN: topicARN}
}

// Publish sends events in batches of 10 (SNS limit).
func (p *SNSPublisher) Publish(ctx context.Context, evs []Event) error {
	for start := 0; start < len(evs); start += 10 {
		end := min(start+10, len(evs))
		entries := make([]snstypes.PublishBatchRequestEntry, 0, end-start)
		for _, ev := range evs[start:end] {
			b, err := json.Marshal(ev)
			if err != nil {
				return fmt.Errorf("events: marshal: %w", err)
			}
			entries = append(entries, snstypes.PublishBatchRequestEntry{
				Id:      aws.String(ev.ID),
				Message: aws.String(string(b)),
				MessageAttributes: map[string]snstypes.MessageAttributeValue{
					"event_type": {DataType: aws.String("String"), StringValue: aws.String(ev.Type)},
				},
			})
		}
		out, err := p.client.PublishBatch(ctx, &sns.PublishBatchInput{TopicArn: aws.String(p.topicARN), PublishBatchRequestEntries: entries})
		if err != nil {
			return fmt.Errorf("events: publish: %w", err)
		}
		if len(out.Failed) > 0 {
			return fmt.Errorf("events: %d of %d messages failed (first: %s)", len(out.Failed), len(entries), aws.ToString(out.Failed[0].Message))
		}
	}
	return nil
}
