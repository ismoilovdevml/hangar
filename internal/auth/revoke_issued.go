package auth

import (
	"context"
	"fmt"

	"github.com/ismoilovdevml/hangar/internal/core"
)

var _ core.TokenRevoker = (*Service)(nil)

// RevokeIssued implements core.TokenRevoker: it revokes a session token
// that Issue minted for the context principal (a client login such as npm
// login). Personal access and service-account tokens are never revoked
// here; tokens minted from the revoked one stop working with it.
func (s *Service) RevokeIssued(ctx context.Context, id string) error {
	p := core.PrincipalFrom(ctx)
	if p.IsAnonymous() || !validUUID(p.ID) {
		return fmt.Errorf("revoke token: %w", core.ErrUnauthenticated)
	}
	if !validUUID(id) {
		return fmt.Errorf("token %q: %w", id, core.ErrNotFound)
	}
	var name string
	err := s.pool.QueryRow(ctx, `
		UPDATE tokens SET revoked_at = $3
		WHERE id = $1 AND user_id = $2 AND kind = 'session' AND revoked_at IS NULL
		RETURNING name`, id, p.ID, s.now()).Scan(&name)
	if err != nil {
		return dbErr(err, fmt.Sprintf("token %q", id))
	}
	s.record(ctx, core.AuditEvent{
		Action: AuditTokenRevoke, Target: "token:" + id, Details: map[string]any{"name": name, "owner": p.Name, "kind": "session"},
	})
	return nil
}
