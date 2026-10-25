package grpcx

import (
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const errorDomain = "acme-card"

// Error builds a gRPC status carrying a machine-readable reason (e.g. "account_closed").
func Error(code codes.Code, reason, msg string) error {
	st := status.New(code, msg)
	if withDetails, err := st.WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: errorDomain}); err == nil {
		return withDetails.Err()
	}
	return st.Err()
}

// Reason extracts the ErrorInfo reason from a gRPC error, or "".
func Reason(err error) string {
	st, ok := status.FromError(err)
	if !ok || st == nil {
		return ""
	}
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			return info.Reason
		}
	}
	return ""
}
