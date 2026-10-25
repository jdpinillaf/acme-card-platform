// Package httpx holds small HTTP helpers shared by the gateway: strict JSON decoding and a
// uniform error envelope. Bodies may not carry fields the endpoint does not use.
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// ErrorBody is the JSON error envelope: {"error":{"code":"...","message":"..."}}.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail carries a machine-readable code and a human message.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// WriteJSON encodes v with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes the standard error envelope.
func WriteError(w http.ResponseWriter, status int, code, msg string) {
	WriteJSON(w, status, ErrorBody{Error: ErrorDetail{Code: code, Message: msg}})
}

// DecodeStrict decodes exactly one JSON value into dst, rejecting unknown fields, trailing data
// and bodies over maxBytes.
func DecodeStrict(r *http.Request, dst any, maxBytes int64) error {
	body := http.MaxBytesReader(nil, r.Body, maxBytes)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return fmt.Errorf("body exceeds %d bytes", maxBytes)
		}
		return fmt.Errorf("invalid json: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("invalid json: body must contain a single value")
	}
	return nil
}
