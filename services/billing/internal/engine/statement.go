package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// DocLine is one movement on a statement.
type DocLine struct {
	EffectiveAt time.Time `json:"effective_at"`
	Type        string    `json:"type"`
	Reference   string    `json:"reference"`
	AmountMinor int64     `json:"amount_minor"`
}

// StatementDoc is the JSON document stored in S3. Field order is fixed by the struct, so the same
// statement always renders the same bytes (and hash).
type StatementDoc struct {
	AccountID              string     `json:"account_id"`
	Currency               string     `json:"currency"`
	Cycle                  int        `json:"cycle"`
	PeriodStart            time.Time  `json:"period_start"`
	PeriodEnd              time.Time  `json:"period_end"`
	DueDate                *time.Time `json:"due_date"`
	PreviousBalanceMinor   int64      `json:"previous_balance_minor"`
	PurchasesMinor         int64      `json:"purchases_minor"`
	RefundsMinor           int64      `json:"refunds_minor"`
	PaymentsMinor          int64      `json:"payments_minor"`
	ReturnsMinor           int64      `json:"returns_minor"`
	InterestMinor          int64      `json:"interest_minor"`
	FeesMinor              int64      `json:"fees_minor"`
	WaiversMinor           int64      `json:"waivers_minor"`
	CollateralAppliedMinor int64      `json:"collateral_applied_minor"`
	NewBalanceMinor        int64      `json:"new_balance_minor"`
	MinimumDueMinor        int64      `json:"minimum_due_minor"`
	PastDueMinor           int64      `json:"past_due_minor"`
	Grace                  bool       `json:"grace"`
	APRBps                 int64      `json:"apr_bps"`
	Buckets                DocBuckets `json:"buckets"`
	PendingPaymentsMinor   int64      `json:"pending_payments_minor"`
	BalanceOnly            bool       `json:"balance_only"`
	Final                  bool       `json:"final"`
	Transactions           []DocLine  `json:"transactions"`
}

// DocBuckets is the closing split of the new balance.
type DocBuckets struct {
	PrincipalMinor int64 `json:"principal_minor"`
	InterestMinor  int64 `json:"interest_minor"`
	FeesMinor      int64 `json:"fees_minor"`
}

// RenderStatement returns the statement JSON and its SHA-256 (hex).
func RenderStatement(doc StatementDoc) ([]byte, string) {
	if doc.Transactions == nil {
		doc.Transactions = []DocLine{}
	}
	b, err := json.Marshal(doc)
	if err != nil { // a struct of plain fields cannot fail to marshal
		panic(err)
	}
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:])
}
