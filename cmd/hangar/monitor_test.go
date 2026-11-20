package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/auth/oidc"
)

// statusDirectory is an LDAP directory that reports its last contact.
type statusDirectory struct {
	at  time.Time
	err error
}

func (statusDirectory) Authenticate(context.Context, string, string) (*auth.ExternalIdentity, error) {
	return nil, errors.New("not used")
}

func (d statusDirectory) Status() (time.Time, error) { return d.at, d.err }

// System status lists the sign-in providers in effect at each run, not the
// ones the configuration file enabled at start: OIDC and LDAP turned on from
// the UI must show up, and turned off must disappear.
func TestAuthProvidersFollowRuntimeSettings(t *testing.T) {
	provider, err := oidc.New(auth.OIDCConfig{Enabled: true, IssuerURL: "https://idp.example", ClientID: "hangar"},
		oidc.Options{RedirectURL: "https://hangar.example/api/v1/auth/oidc/callback"})
	if err != nil {
		t.Fatal(err)
	}
	contacted := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		oidc      *oidc.Provider
		dir       auth.Directory
		wantNames []string
	}{
		{"nothing enabled", nil, nil, nil},
		{"OIDC enabled from the UI", provider, nil, []string{"OIDC"}},
		{"LDAP enabled from the UI", nil, statusDirectory{at: contacted}, []string{"LDAP"}},
		{"both", provider, statusDirectory{at: contacted}, []string{"OIDC", "LDAP"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := authProviders(func() *oidc.Provider { return tc.oidc }, func() auth.Directory { return tc.dir })
			if len(got) != len(tc.wantNames) {
				t.Fatalf("providers = %d, want %v", len(got), tc.wantNames)
			}
			for i, p := range got {
				if p.Name != tc.wantNames[i] {
					t.Errorf("provider %d = %s, want %s", i, p.Name, tc.wantNames[i])
				}
				if p.Name == "LDAP" {
					if ok, at, err := p.Status(); !ok || !at.Equal(contacted) || err != nil {
						t.Errorf("LDAP status = %v %v %v", ok, at, err)
					}
				}
			}
		})
	}
	if got := authProviders(nil, directoryOf(nil)); len(got) != 0 {
		t.Fatalf("no auth service: %d providers", len(got))
	}
}
