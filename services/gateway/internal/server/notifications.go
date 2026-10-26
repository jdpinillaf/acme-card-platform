package server

import (
	"bytes"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	accountsv1 "github.com/acme/acme-card/gen/go/acme/accounts/v1"
	notificationsv1 "github.com/acme/acme-card/gen/go/acme/notifications/v1"
	"github.com/acme/acme-card/pkg/auth"
	"github.com/acme/acme-card/pkg/httpx"
	"github.com/acme/acme-card/pkg/idempotency"
)

// customerOf resolves the account's customer (preferences belong to the customer).
func (s *server) customerOf(w http.ResponseWriter, r *http.Request) (string, bool) {
	res, err := s.Accounts.GetAccount(r.Context(), &accountsv1.GetAccountRequest{AccountId: chi.URLParam(r, "accountID")})
	if err != nil {
		writeGRPCError(w, r, err)
		return "", false
	}
	return res.Account.GetCustomerId(), true
}

func preferenceJSON(p *notificationsv1.Preference) map[string]any {
	return map[string]any{"type": p.Type, "push": p.Push, "sms": p.Sms, "email": p.Email, "mandatory": p.Mandatory}
}

func (s *server) getNotificationPreferences(w http.ResponseWriter, r *http.Request) {
	customer, ok := s.customerOf(w, r)
	if !ok {
		return
	}
	res, err := s.Notifications.GetPreferences(r.Context(), &notificationsv1.GetPreferencesRequest{CustomerId: customer})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(res.Preferences))
	for _, p := range res.Preferences {
		out = append(out, preferenceJSON(p))
	}
	httpx.WriteJSON(w, 200, map[string]any{"preferences": out})
}

func (s *server) setNotificationPreference(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Type  string `json:"type"`
		Push  bool   `json:"push"`
		SMS   bool   `json:"sms"`
		Email bool   `json:"email"`
	}
	if err := httpx.DecodeStrict(r, &body, maxBody); err != nil {
		httpx.WriteError(w, 400, "invalid_body", err.Error())
		return
	}
	customer, ok := s.customerOf(w, r)
	if !ok {
		return
	}
	res, err := s.Notifications.SetPreference(r.Context(), &notificationsv1.SetPreferenceRequest{CustomerId: customer, Type: body.Type, Push: body.Push,
		Sms: body.SMS, Email: body.Email, Meta: meta(r, auth.Subject(r))})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	httpx.WriteJSON(w, 200, preferenceJSON(res))
}

func (s *server) listNotifications(w http.ResponseWriter, r *http.Request) {
	var after int64
	if v := r.URL.Query().Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			httpx.WriteError(w, 400, "invalid_request", "after must be a notification id")
			return
		}
		after = n
	}
	res, err := s.Notifications.ListInbox(r.Context(), &notificationsv1.ListInboxRequest{AccountId: chi.URLParam(r, "accountID"), AfterId: after})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(res.Items))
	for _, it := range res.Items {
		items = append(items, map[string]any{"id": it.Id, "channel": it.Channel, "type": it.Type, "text": it.Text, "created_at": ts(it.CreatedAt)})
	}
	httpx.WriteJSON(w, 200, map[string]any{"items": items})
}

func subscriptionJSON(sub *notificationsv1.Subscription) map[string]any {
	types := sub.EventTypes
	if types == nil {
		types = []string{}
	}
	return map[string]any{"id": sub.Id, "name": sub.Name, "url": sub.Url, "event_types": types, "active": sub.Active,
		"created_at": ts(sub.CreatedAt), "disabled_at": ts(sub.DisabledAt)}
}

// createWebhookSubscription registers a partner (support role). The secret is shown in this
// response only: the idempotency store keeps the response without it, so a replay never repeats it.
func (s *server) createWebhookSubscription(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name       string   `json:"name"`
		URL        string   `json:"url"`
		EventTypes []string `json:"event_types"`
	}
	if err := httpx.DecodeStrict(r, &body, maxBody); err != nil {
		httpx.WriteError(w, 400, "invalid_body", err.Error())
		return
	}
	res, err := s.Notifications.CreateSubscription(r.Context(), &notificationsv1.CreateSubscriptionRequest{Name: body.Name, Url: body.URL,
		EventTypes: body.EventTypes, Meta: meta(r, auth.Subject(r))})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	secret := []byte(`"secret":"` + res.Secret + `"`)
	idempotency.RedactStored(r.Context(), func(b []byte) []byte {
		return bytes.ReplaceAll(b, secret, []byte(`"secret":"[shown once]"`))
	})
	out := subscriptionJSON(res.Subscription)
	out["secret"] = res.Secret
	httpx.WriteJSON(w, 201, out)
}

func (s *server) listWebhookSubscriptions(w http.ResponseWriter, r *http.Request) {
	res, err := s.Notifications.ListSubscriptions(r.Context(), &notificationsv1.ListSubscriptionsRequest{})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(res.Subscriptions))
	for _, sub := range res.Subscriptions {
		out = append(out, subscriptionJSON(sub))
	}
	httpx.WriteJSON(w, 200, map[string]any{"subscriptions": out})
}

func (s *server) disableWebhookSubscription(w http.ResponseWriter, r *http.Request) {
	res, err := s.Notifications.DisableSubscription(r.Context(), &notificationsv1.DisableSubscriptionRequest{Id: chi.URLParam(r, "subscriptionID"),
		Meta: meta(r, auth.Subject(r))})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	httpx.WriteJSON(w, 200, subscriptionJSON(res))
}

func (s *server) listWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	res, err := s.Notifications.ListDeliveries(r.Context(), &notificationsv1.ListDeliveriesRequest{SubscriptionId: chi.URLParam(r, "subscriptionID")})
	if err != nil {
		writeGRPCError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(res.Deliveries))
	for _, d := range res.Deliveries {
		out = append(out, map[string]any{"id": d.Id, "event_id": d.EventId, "event_type": d.EventType, "status": d.Status, "attempts": d.Attempts,
			"last_error": d.LastError, "next_attempt_at": ts(d.NextAttemptAt), "updated_at": ts(d.UpdatedAt)})
	}
	httpx.WriteJSON(w, 200, map[string]any{"deliveries": out})
}
