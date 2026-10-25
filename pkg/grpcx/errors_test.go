package grpcx

import (
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestErrorCarriesReason(t *testing.T) {
	err := Error(codes.FailedPrecondition, "account_closed", "account is closed")
	if status.Code(err) != codes.FailedPrecondition || Reason(err) != "account_closed" {
		t.Fatalf("code=%v reason=%q", status.Code(err), Reason(err))
	}
	if Reason(errors.New("plain")) != "" {
		t.Fatal("plain errors have no reason")
	}
}
