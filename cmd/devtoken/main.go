// devtoken mints a development JWT (HS256) for the gateway. Never use in production.
//
//	JWT_HS256_SECRET=... go run ./cmd/devtoken --sub cust-1 --accounts acc-1,acc-2 --ttl 24h
//	JWT_HS256_SECRET=... go run ./cmd/devtoken --sub agent-1 --role support
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/acme/acme-card/pkg/auth"
)

func main() {
	sub := flag.String("sub", "dev-customer", "token subject (customer id)")
	accounts := flag.String("accounts", "", "comma-separated account ids the token may act on")
	ttl := flag.Duration("ttl", 24*time.Hour, "token lifetime")
	role := flag.String("role", "", "optional role claim (support)")
	flag.Parse()

	secret := os.Getenv("JWT_HS256_SECRET")
	if len(secret) < 32 {
		fmt.Fprintln(os.Stderr, "JWT_HS256_SECRET must be set (>= 32 bytes)")
		os.Exit(2)
	}
	var accs []string
	for _, a := range strings.Split(*accounts, ",") {
		if a = strings.TrimSpace(a); a != "" {
			accs = append(accs, a)
		}
	}
	tok, err := auth.SignWithRole([]byte(secret), *sub, accs, *role, *ttl)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(tok)
}
