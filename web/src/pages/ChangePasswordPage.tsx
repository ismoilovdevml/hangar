import { useId, useMemo, useState, type FormEvent } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { getRouteApi, useRouter } from '@tanstack/react-router';
import { ApiRequestError, apiFetch } from '../api/client';
import { leaveAfterLogout, queryKeys, useLogout, useSession, useSystemStatus } from '../api/hooks';
import { IconCheck, IconLogo } from '../components/icons';
import { DEFAULT_PASSWORD_POLICY, passwordChecks } from '../lib/passwordPolicy';
import { safeRedirect } from '../lib/redirect';
import './changePassword.css';

const route = getRouteApi('/change-password');

function changeError(err: unknown): string {
  if (err instanceof ApiRequestError) {
    const msg = (err.body?.message ?? '').replace(/: invalid argument$/, '');
    if (err.status === 400 && msg.includes('current password is incorrect')) return 'The current password is wrong.';
    if (err.status === 423) {
      const mins = err.retryAfter ? Math.ceil(err.retryAfter / 60) : null;
      return `Your account is locked after repeated wrong passwords. ${mins ? `Try again in ${mins} min` : 'Try again later'}, or ask an administrator to unlock it.`;
    }
    if (msg) return msg.charAt(0).toUpperCase() + msg.slice(1) + (msg.endsWith('.') ? '' : '.');
    return 'The password could not be changed. Please try again.';
  }
  return 'Cannot reach the server. Check your connection and try again.';
}

/**
 * Shown instead of the app to a user who signed in with a password they must
 * replace (the generated first administrator, a one-time password, or an
 * administrator's request). The server answers nothing else until it is done.
 */
export function ChangePasswordPage() {
  const { redirect } = route.useSearch();
  const router = useRouter();
  const qc = useQueryClient();
  const session = useSession();
  const status = useSystemStatus();
  const logout = useLogout();

  const [current, setCurrent] = useState('');
  const [next, setNext] = useState('');
  const [confirm, setConfirm] = useState('');
  const [show, setShow] = useState(false);
  const [submitted, setSubmitted] = useState(false);
  const ids = { intro: useId(), checks: useId(), error: useId(), current: useId() };

  const user = session.data?.user;
  const policy = session.data?.password_policy ?? DEFAULT_PASSWORD_POLICY;
  const checks = useMemo(() => passwordChecks(policy, user?.username ?? '', current, next, confirm), [policy, user?.username, current, next, confirm]);
  const unmet = checks.filter((c) => !c.met);

  const change = useMutation({
    mutationFn: (body: { current_password: string; new_password: string }) => apiFetch<void>('/me/password', { method: 'PUT', body }),
    onSuccess: async () => {
      // Nothing cached while the session was limited may be reused.
      qc.removeQueries({ predicate: (q) => !['auth', 'system', 'session'].includes(String(q.queryKey[0])) });
      await qc.invalidateQueries({ queryKey: queryKeys.session });
      router.history.push(safeRedirect(redirect));
    },
  });

  const onSubmit = (e: FormEvent) => {
    e.preventDefault();
    setSubmitted(true);
    if (!current || unmet.length > 0 || change.isPending) return;
    change.mutate({ current_password: current, new_password: next });
  };

  const signOut = () =>
    logout.mutate(undefined, { onSettled: (res) => leaveAfterLogout(res, () => void router.navigate({ to: '/login', replace: true })) });

  const currentMissing = submitted && !current;
  const nextInvalid = submitted && unmet.some((c) => c.id !== 'match');
  const confirmInvalid = submitted && unmet.some((c) => c.id === 'match');
  const errorText = change.isError
    ? changeError(change.error)
    : submitted && (currentMissing || unmet.length > 0)
      ? currentMissing
        ? 'Enter your current password.'
        : 'The new password does not meet every requirement yet.'
      : null;
  const type = show ? 'text' : 'password';

  return (
    <div className="login wall wall--center">
      <main className="login__main">
        <form className="login-card glass" onSubmit={onSubmit} aria-labelledby="change-password-title" aria-describedby={ids.intro} noValidate>
          <div className="login-card__intro">
            <span className="brand__mark brand__mark--lg login-card__mark" aria-hidden="true">
              <IconLogo size={20} />
            </span>
            <h1 id="change-password-title" className="login-card__title">
              Set a new password
            </h1>
            <p className="login-card__sub">
              Signed in as <strong className="mono">{user?.username ?? '…'}</strong>
            </p>
          </div>
          <p id={ids.intro} className="change-password__lead">
            The password you signed in with was issued to you and must be replaced before you can continue. Choose one only you
            know; the old one stops working.
          </p>

          <label className="field">
            <span className="field__label">Current password</span>
            <input
              id={ids.current}
              className="input"
              type={type}
              name="current-password"
              autoComplete="current-password"
              required
              value={current}
              onChange={(e) => setCurrent(e.target.value)}
              aria-invalid={currentMissing || undefined}
            />
          </label>
          <label className="field">
            <span className="field__label">New password</span>
            <input
              className="input"
              type={type}
              name="new-password"
              autoComplete="new-password"
              required
              value={next}
              onChange={(e) => setNext(e.target.value)}
              aria-invalid={nextInvalid || undefined}
              aria-describedby={ids.checks}
            />
          </label>
          <label className="field">
            <span className="field__label">Confirm new password</span>
            <input
              className="input"
              type={type}
              name="confirm-password"
              autoComplete="new-password"
              required
              value={confirm}
              onChange={(e) => setConfirm(e.target.value)}
              aria-invalid={confirmInvalid || undefined}
            />
          </label>

          <label className="checkbox change-password__show">
            <input type="checkbox" checked={show} onChange={(e) => setShow(e.target.checked)} />
            Show passwords
          </label>

          <ul id={ids.checks} className="policy-checks" aria-label="Password requirements">
            {checks.map((c) => (
              <li key={c.id} className={c.met ? 'policy-checks__item is-met' : 'policy-checks__item'}>
                <span className="policy-checks__mark" aria-hidden="true">
                  {c.met ? <IconCheck size={12} /> : null}
                </span>
                {c.label}
                <span className="visually-hidden">{c.met ? ' (met)' : ' (not met yet)'}</span>
              </li>
            ))}
          </ul>

          {errorText && (
            <p id={ids.error} className="form-error" role="alert">
              {errorText}
            </p>
          )}

          <button type="submit" className="button button--dark button--block" disabled={change.isPending || change.isSuccess}>
            {change.isPending || change.isSuccess ? 'Saving…' : 'Set password and continue'}
          </button>
          <button type="button" className="button button--secondary button--block" onClick={signOut} disabled={logout.isPending}>
            {logout.isPending ? 'Signing out…' : 'Sign out'}
          </button>
        </form>
      </main>

      <footer className="login__footer">
        <span>{status.data ? `Hangar ${status.data.version}` : 'Hangar'}</span>
      </footer>
    </div>
  );
}
