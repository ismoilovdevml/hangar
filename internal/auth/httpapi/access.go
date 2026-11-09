package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/core"
)

// ---------------------------------------------------------------- /users/{id}/...

func (h *handler) unlockUser(w http.ResponseWriter, r *http.Request) {
	u, err := h.svc.UnlockUser(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toMember(u, h.svc.Now()))
}

func (h *handler) disableUser(w http.ResponseWriter, r *http.Request) { h.setDisabled(w, r, true) }
func (h *handler) enableUser(w http.ResponseWriter, r *http.Request)  { h.setDisabled(w, r, false) }

func (h *handler) setDisabled(w http.ResponseWriter, r *http.Request, disabled bool) {
	u, err := h.svc.SetUserDisabled(r.Context(), chi.URLParam(r, "id"), disabled)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toMember(u, h.svc.Now()))
}

// passwordReset is schema PasswordReset.
type passwordReset struct {
	Password string `json:"password"`
}

func (h *handler) resetPassword(w http.ResponseWriter, r *http.Request) {
	var in passwordReset
	if err := decode(w, r, &in); err != nil {
		h.writeError(w, r, err)
		return
	}
	if err := h.svc.ResetPassword(r.Context(), chi.URLParam(r, "id"), in.Password); err != nil {
		h.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) requirePasswordChange(w http.ResponseWriter, r *http.Request) {
	u, err := h.svc.RequirePasswordChange(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toMember(u, h.svc.Now()))
}

func (h *handler) userPermissions(w http.ResponseWriter, r *http.Request) {
	ep, err := h.svc.EffectivePermissions(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ep)
}

// currentSessionID is the id of the browser session making the request, or "".
func currentSessionID(r *http.Request) string {
	if ra := authFrom(r.Context()); ra != nil && ra.session != nil {
		return ra.session.ID
	}
	return ""
}

func (h *handler) writeSessions(w http.ResponseWriter, r *http.Request, userID string) {
	items, err := h.svc.ListSessions(r.Context(), userID, currentSessionID(r))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": nonNil(items)})
}

// sessionsEnded is schema SessionsEnded.
type sessionsEnded struct {
	Ended int64 `json:"ended"`
}

func (h *handler) userSessions(w http.ResponseWriter, r *http.Request) {
	h.writeSessions(w, r, chi.URLParam(r, "id"))
}

func (h *handler) revokeUserSessions(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	keep := ""
	if id == core.PrincipalFrom(r.Context()).ID {
		keep = currentSessionID(r) // never sign the caller out by accident
	}
	n, err := h.svc.RevokeSessions(r.Context(), id, keep)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sessionsEnded{Ended: n})
}

func (h *handler) revokeUserSession(w http.ResponseWriter, r *http.Request) {
	h.revokeOne(w, r, chi.URLParam(r, "id"))
}

// revokeOne ends one session; ending the current one also clears its cookies.
func (h *handler) revokeOne(w http.ResponseWriter, r *http.Request, userID string) {
	sid := chi.URLParam(r, "sid")
	if err := h.svc.RevokeSession(r.Context(), userID, sid); err != nil {
		h.writeError(w, r, err)
		return
	}
	if sid == currentSessionID(r) {
		h.clearCookies(w, r)
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------- /me/...

func (h *handler) updateProfile(w http.ResponseWriter, r *http.Request) {
	var in auth.ProfileInput
	if err := decode(w, r, &in); err != nil {
		h.writeError(w, r, err)
		return
	}
	u, err := h.svc.UpdateProfile(r.Context(), in)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toMember(u, h.svc.Now()))
}

// passwordChange is schema PasswordChange.
type passwordChange struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

func (h *handler) changePassword(w http.ResponseWriter, r *http.Request) {
	var in passwordChange
	if err := decode(w, r, &in); err != nil {
		h.writeError(w, r, err)
		return
	}
	if in.CurrentPassword == "" || in.NewPassword == "" {
		writeErrorBody(w, http.StatusBadRequest, "invalid_argument", "current_password and new_password are required", 0)
		return
	}
	if err := h.svc.ChangePassword(r.Context(), in.CurrentPassword, in.NewPassword, currentSessionID(r)); err != nil {
		h.writeError(w, r, err)
		return
	}
	// A new credential gets a new session id (session fixation); for a user
	// who had to change their password, the new session is unrestricted.
	if ra := authFrom(r.Context()); ra != nil && ra.session != nil {
		if _, err := h.startSession(w, r, core.PrincipalFrom(r.Context()), ra.session.Remember); err != nil {
			h.writeError(w, r, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) mySessions(w http.ResponseWriter, r *http.Request) {
	h.writeSessions(w, r, core.PrincipalFrom(r.Context()).ID)
}

// revokeMySessions ends every session of the caller except the current one.
func (h *handler) revokeMySessions(w http.ResponseWriter, r *http.Request) {
	n, err := h.svc.RevokeSessions(r.Context(), core.PrincipalFrom(r.Context()).ID, currentSessionID(r))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sessionsEnded{Ended: n})
}

func (h *handler) revokeMySession(w http.ResponseWriter, r *http.Request) {
	h.revokeOne(w, r, core.PrincipalFrom(r.Context()).ID)
}

// ---------------------------------------------------------------- /roles/{name}/...

func (h *handler) getRole(w http.ResponseWriter, r *http.Request) {
	role, err := h.svc.GetRole(r.Context(), chi.URLParam(r, "name"))
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, role)
}

// roleMember is schema RoleMemberInput.
type roleMember struct {
	UserID string `json:"user_id"`
}

func (h *handler) bindRole(w http.ResponseWriter, r *http.Request) {
	var in roleMember
	if err := decode(w, r, &in); err != nil {
		h.writeError(w, r, err)
		return
	}
	if err := h.svc.BindRole(r.Context(), chi.URLParam(r, "name"), in.UserID); err != nil {
		h.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) unbindRole(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.UnbindRole(r.Context(), chi.URLParam(r, "name"), chi.URLParam(r, "id")); err != nil {
		h.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
