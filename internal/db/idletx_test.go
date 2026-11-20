package db

import (
	"context"
	"net/url"
	"os"
	"testing"
)

func TestIdleInTransactionTimeout(t *testing.T) {
	base := os.Getenv(EnvTestDatabaseURL)
	if base == "" {
		t.Skipf("%s not set; skipping database integration test", EnvTestDatabaseURL)
	}
	withParam := func(v string) string {
		u, err := url.Parse(base)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		q.Set(idleInTxParam, v)
		u.RawQuery = q.Encode()
		return u.String()
	}
	for _, tc := range []struct {
		name, dsn, want string
	}{
		{"default", base, "5min"},
		{"set in the url", withParam("90s"), "90s"},
		{"disabled in the url", withParam("0"), "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := poolFor(t, tc.dsn, 2)
			var got string
			if err := pool.QueryRow(context.Background(), "SHOW "+idleInTxParam).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("%s = %q, want %q", idleInTxParam, got, tc.want)
			}
		})
	}
}
