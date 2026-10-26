package notify

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	notificationsv1 "github.com/acme/acme-card/gen/go/acme/notifications/v1"
	"github.com/acme/acme-card/pkg/grpcx"
	"github.com/acme/acme-card/services/notifications/internal/rules"
)

func internal(err error) error {
	if _, ok := status.FromError(err); ok {
		return err
	}
	return status.Error(codes.Internal, err.Error())
}

// GetPreferences lists every type with its channels (no row = all on).
func (s *Service) GetPreferences(ctx context.Context, req *notificationsv1.GetPreferencesRequest) (*notificationsv1.GetPreferencesResponse, error) {
	if req.CustomerId == "" {
		return nil, status.Error(codes.InvalidArgument, "customer_id is required")
	}
	stored := map[string]*notificationsv1.Preference{}
	rows, err := s.pool.Query(ctx, `SELECT type, push, sms, email FROM preferences WHERE customer_id=$1`, req.CustomerId)
	if err != nil {
		return nil, internal(err)
	}
	for rows.Next() {
		p := &notificationsv1.Preference{}
		if err := rows.Scan(&p.Type, &p.Push, &p.Sms, &p.Email); err != nil {
			rows.Close()
			return nil, internal(err)
		}
		stored[p.Type] = p
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, internal(err)
	}
	res := &notificationsv1.GetPreferencesResponse{}
	for _, typ := range rules.AllTypes {
		p, ok := stored[typ]
		if !ok {
			p = &notificationsv1.Preference{Type: typ, Push: true, Sms: true, Email: true}
		}
		p.Mandatory = rules.MandatoryTypes[typ]
		res.Preferences = append(res.Preferences, p)
	}
	return res, nil
}

// SetPreference stores the channels of one type; a mandatory type keeps at least one.
func (s *Service) SetPreference(ctx context.Context, req *notificationsv1.SetPreferenceRequest) (*notificationsv1.Preference, error) {
	if req.CustomerId == "" {
		return nil, status.Error(codes.InvalidArgument, "customer_id is required")
	}
	switch err := rules.ValidatePreference(req.Type, req.Push, req.Sms, req.Email); {
	case errors.Is(err, rules.ErrUnknownType):
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_notification_type", "unknown notification type")
	case errors.Is(err, rules.ErrMandatory):
		return nil, grpcx.Error(codes.FailedPrecondition, "mandatory_notification", "this notification is mandatory: keep at least one channel on")
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO preferences (customer_id, type, push, sms, email) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (customer_id, type) DO UPDATE SET push=EXCLUDED.push, sms=EXCLUDED.sms, email=EXCLUDED.email, updated_at=now()`,
		req.CustomerId, req.Type, req.Push, req.Sms, req.Email); err != nil {
		return nil, internal(err)
	}
	return &notificationsv1.Preference{Type: req.Type, Push: req.Push, Sms: req.Sms, Email: req.Email, Mandatory: rules.MandatoryTypes[req.Type]}, nil
}

// ListInbox returns the newest delivered customer notifications of an account (after after_id).
func (s *Service) ListInbox(ctx context.Context, req *notificationsv1.ListInboxRequest) (*notificationsv1.ListInboxResponse, error) {
	limit := req.Limit
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `SELECT id, channel, type, text, created_at FROM inbox WHERE account_id=$1 AND id > $2 ORDER BY id DESC LIMIT $3`,
		req.AccountId, req.AfterId, limit)
	if err != nil {
		return nil, internal(err)
	}
	defer rows.Close()
	res := &notificationsv1.ListInboxResponse{}
	for rows.Next() {
		it := &notificationsv1.InboxItem{}
		var at time.Time
		if err := rows.Scan(&it.Id, &it.Channel, &it.Type, &it.Text, &at); err != nil {
			return nil, internal(err)
		}
		it.CreatedAt = timestamppb.New(at)
		res.Items = append(res.Items, it)
	}
	return res, internal0(rows.Err())
}

func internal0(err error) error {
	if err == nil {
		return nil
	}
	return internal(err)
}

const subscriptionCols = `id::text, name, url, event_types, active, created_at, disabled_at`

func scanSubscription(row pgx.Row) (*notificationsv1.Subscription, error) {
	sub := &notificationsv1.Subscription{}
	var created time.Time
	var disabled *time.Time
	if err := row.Scan(&sub.Id, &sub.Name, &sub.Url, &sub.EventTypes, &sub.Active, &created, &disabled); err != nil {
		return nil, err
	}
	sub.CreatedAt = timestamppb.New(created)
	if disabled != nil {
		sub.DisabledAt = timestamppb.New(*disabled)
	}
	return sub, nil
}

// CreateSubscription registers a partner. The secret is generated here, returned once and stored
// sealed.
func (s *Service) CreateSubscription(ctx context.Context, req *notificationsv1.CreateSubscriptionRequest) (*notificationsv1.CreateSubscriptionResponse, error) {
	if req.Name == "" || len(req.Name) > 100 {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_name", "name is required (at most 100 characters)")
	}
	if err := validateURL(ctx, req.Url, s.cfg.AllowedHTTPHosts, s.lookup); err != nil {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_url", err.Error())
	}
	if len(req.EventTypes) == 0 {
		return nil, grpcx.Error(codes.InvalidArgument, "invalid_event_type", "at least one event type is required")
	}
	for _, t := range req.EventTypes {
		if !rules.PartnerEventAllowed(t) {
			return nil, grpcx.Error(codes.InvalidArgument, "invalid_event_type", "event type not available to partners: "+t)
		}
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, internal(err)
	}
	secret := hex.EncodeToString(raw)
	sealed, err := s.box.Seal([]byte(secret))
	if err != nil {
		return nil, internal(err)
	}
	sub, err := scanSubscription(s.pool.QueryRow(ctx, `INSERT INTO subscriptions (id, name, url, event_types, secret_sealed) VALUES ($1,$2,$3,$4,$5)
		RETURNING `+subscriptionCols, uuid.Must(uuid.NewV7()), req.Name, req.Url, req.EventTypes, sealed))
	if err != nil {
		return nil, internal(err)
	}
	return &notificationsv1.CreateSubscriptionResponse{Subscription: sub, Secret: secret}, nil
}

// ListSubscriptions lists partners (never their secrets).
func (s *Service) ListSubscriptions(ctx context.Context, _ *notificationsv1.ListSubscriptionsRequest) (*notificationsv1.ListSubscriptionsResponse, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+subscriptionCols+` FROM subscriptions ORDER BY created_at DESC`)
	if err != nil {
		return nil, internal(err)
	}
	defer rows.Close()
	res := &notificationsv1.ListSubscriptionsResponse{}
	for rows.Next() {
		sub, err := scanSubscription(rows)
		if err != nil {
			return nil, internal(err)
		}
		res.Subscriptions = append(res.Subscriptions, sub)
	}
	return res, internal0(rows.Err())
}

// DisableSubscription stops a partner (kept for the audit trail).
func (s *Service) DisableSubscription(ctx context.Context, req *notificationsv1.DisableSubscriptionRequest) (*notificationsv1.Subscription, error) {
	if _, err := uuid.Parse(req.Id); err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid subscription id")
	}
	sub, err := scanSubscription(s.pool.QueryRow(ctx, `UPDATE subscriptions SET active=false, disabled_at=coalesce(disabled_at, now()) WHERE id=$1
		RETURNING `+subscriptionCols, req.Id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "subscription not found")
	}
	if err != nil {
		return nil, internal(err)
	}
	return sub, nil
}

// ListDeliveries lists a partner's latest deliveries.
func (s *Service) ListDeliveries(ctx context.Context, req *notificationsv1.ListDeliveriesRequest) (*notificationsv1.ListDeliveriesResponse, error) {
	if _, err := uuid.Parse(req.SubscriptionId); err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid subscription id")
	}
	limit := req.Limit
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `SELECT id::text, event_id::text, event_type, status, attempts, coalesce(last_error,''), next_attempt_at, updated_at
		FROM deliveries WHERE target=$1 ORDER BY created_at DESC LIMIT $2`, "webhook:"+req.SubscriptionId, limit)
	if err != nil {
		return nil, internal(err)
	}
	defer rows.Close()
	res := &notificationsv1.ListDeliveriesResponse{}
	for rows.Next() {
		d := &notificationsv1.Delivery{}
		var next, updated time.Time
		if err := rows.Scan(&d.Id, &d.EventId, &d.EventType, &d.Status, &d.Attempts, &d.LastError, &next, &updated); err != nil {
			return nil, internal(err)
		}
		d.NextAttemptAt, d.UpdatedAt = timestamppb.New(next), timestamppb.New(updated)
		res.Deliveries = append(res.Deliveries, d)
	}
	return res, internal0(rows.Err())
}
