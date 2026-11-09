import { useId, useState, type FormEvent } from 'react';
import { getRouteApi, useRouter } from '@tanstack/react-router';
import { ApiRequestError } from '../api/client';
import { useAuthProviders, useLogin, useSystemStatus } from '../api/hooks';
import { IconLogo } from '../components/icons';
import { DOCS } from '../lib/docs';
import { instanceName, versionText } from '../lib/format';
import { useNow } from '../lib/useNow';

const route = getRouteApi('/login');

const API_URL = '/api-docs';

/** Messages for `?error=` set by the single sign-on callback. */
const SSO_ERRORS: Record<string, string> = {
  sso_unavailable: 'Single sign-on is unavailable right now. Try again later or use a local account.',
  sso_state: 'The sign-in expired or was started in another tab. Please try again.',
  sso_failed: 'Single sign-on failed. Please try again.',
  access_denied: 'Single sign-on was cancelled or denied.',
  group_denied: 'Your account is not in a group allowed to use this registry. Ask an administrator for access.',
  account_conflict: 'A local account with the same username already exists. Ask an administrator to resolve it.',
  not_provisioned: 'Your account has not been set up in this registry. Ask an administrator for access.',
  invalid_profile: 'Your identity provider did not send a usable username. Ask an administrator to check the SSO setup.',
  account_disabled: 'Your account is disabled. Ask an administrator.',
};

function waitText(secs: number): string {
  if (secs <= 0) return 'now';
  if (secs < 60) return `in ${secs} s`;
  return `in ${Math.ceil(secs / 60)} min`;
}

/**
 * What went wrong. The text stays the same while a lockout or rate limit runs
 * (it is announced once); the submit button carries the live countdown.
 */
/** An API message as a sentence: "too many x; try again later" -> "Too many x. Try again later." */
function sentence(message: string | undefined): string | null {
  const m = message?.trim();
  if (!m) return null;
  const text = m.replace(/;\s+(\w)/g, (_, c: string) => `. ${c.toUpperCase()}`);
  return `${text.charAt(0).toUpperCase()}${text.slice(1)}${/[.!?]$/.test(text) ? '' : '.'}`;
}

function loginError(err: unknown, expired: boolean): string {
  if (err instanceof ApiRequestError) {
    const wait = err.retryAfter;
    if (err.status === 401) return 'Wrong username or password.';
    if (err.status === 423) {
      if (expired) return 'The lock has expired. You can try again.';
      // Lockout is per username and address, and unknown accounts get the same answer: never claim "this account".
      const said = sentence(err.body?.message) ?? 'Too many failed sign-ins from this address. Try again later.';
      return wait ? `${said.replace(/\s*Try again later\.$/i, '')} Try again ${waitText(wait)}.` : said;
    }
    if (err.status === 429) {
      if (expired) return 'You can try again now.';
      return `Too many sign-in attempts from this address. ${wait ? `Try again ${waitText(wait)}.` : 'Wait a moment and try again.'}`;
    }
    if (err.status === 403) {
      if (err.body?.code === 'forbidden' && err.body.message?.includes('cross-origin'))
        return 'The server rejected this sign-in request. Open Hangar from its own address and try again.';
      return err.body?.message ?? 'You are not allowed to sign in.';
    }
    return err.body?.message ?? 'Sign-in failed. Please try again.';
  }
  return 'Cannot reach the server. Check your connection and try again.';
}

export function LoginPage() {
  const { redirect, error: ssoError } = route.useSearch();
  const router = useRouter();
  const providers = useAuthProviders();
  const status = useSystemStatus();
  const login = useLogin();

  const [realm, setRealm] = useState<'local' | 'ldap'>('local');
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [remember, setRemember] = useState(false);
  const errorId = useId();

  const p = providers.data;
  const sys = status.data;
  const instance = instanceName(sys?.instance_name);
  const external = Boolean(p?.oidc || p?.ldap.enabled || (p?.saml.enabled && p.saml.start_url));

  const [missing, setMissing] = useState(false);
  // When a lockout (423) or rate limit (429) ends, as epoch milliseconds.
  const [retry, setRetry] = useState<{ at: number; secs: number } | null>(null);
  const now = useNow(1000).getTime();
  // The clock ticks once a second, so it can lag the moment the wait started: clamp.
  const secsLeft = retry === null ? null : Math.min(retry.secs, Math.max(0, Math.ceil((retry.at - now) / 1000)));
  const waiting = secsLeft !== null && secsLeft > 0;

  const onSubmit = (e: FormEvent) => {
    e.preventDefault();
    if (waiting) return;
    if (!username.trim() || !password) {
      setMissing(true);
      return;
    }
    setMissing(false);
    login.mutate(
      { username: username.trim(), password, remember, realm },
      {
        onSuccess: () => {
          setRetry(null);
          // Only follow same-origin, in-app redirects (path plus query string).
          const target = redirect && redirect.startsWith('/') && !redirect.startsWith('//') ? redirect : '/';
          router.history.push(target);
        },
        onError: (err) => {
          const wait = err instanceof ApiRequestError && (err.status === 423 || err.status === 429) ? err.retryAfter : null;
          setRetry(wait === null ? null : { at: Date.now() + wait * 1000, secs: wait });
        },
      },
    );
  };

  const errorText = missing
    ? 'Enter your username and password.'
    : login.isError
      ? loginError(login.error, secsLeft === 0)
      : ssoError
        ? (SSO_ERRORS[ssoError] ?? 'Single sign-on failed. Please try again.')
        : null;

  return (
    <div className="login wall wall--center">
      <main className="login__main">
        <form className="login-card glass" onSubmit={onSubmit} aria-labelledby="login-title" noValidate>
          <div className="login-card__intro">
            <span className="brand__mark brand__mark--lg login-card__mark" aria-hidden="true">
              <IconLogo size={20} />
            </span>
            <h1 id="login-title" className="login-card__title">
              Welcome back
            </h1>
            <p className="login-card__sub">Sign in to {instance}</p>
          </div>

          {p?.oidc && (
            <a className="button button--primary button--block" href={p.oidc.start_url}>
              Continue with {p.oidc.display_name}
            </a>
          )}

          {(p?.ldap.enabled || p?.saml.enabled) && (
            <div className="login-card__alt">
              {p.ldap.enabled && (
                <button
                  type="button"
                  className="button button--secondary"
                  aria-pressed={realm === 'ldap'}
                  onClick={() => setRealm((r) => (r === 'ldap' ? 'local' : 'ldap'))}
                >
                  LDAP
                </button>
              )}
              {p.saml.enabled && p.saml.start_url && (
                <a className="button button--secondary" href={p.saml.start_url}>
                  SAML
                </a>
              )}
            </div>
          )}

          {external && (
            <div className="divider">{realm === 'ldap' ? 'with your directory account' : 'or with a local account'}</div>
          )}

          <label className="field">
            <span className="field__label">{realm === 'ldap' ? 'Directory username' : 'Username'}</span>
            <input
              className="input"
              type="text"
              name="username"
              autoComplete="username"
              autoCapitalize="off"
              spellCheck={false}
              required
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              aria-invalid={errorText ? true : undefined}
              aria-describedby={errorText ? errorId : undefined}
            />
          </label>
          <label className="field">
            <span className="field__label">Password</span>
            <input
              className="input"
              type="password"
              name="password"
              autoComplete="current-password"
              required
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              aria-invalid={errorText ? true : undefined}
              aria-describedby={errorText ? errorId : undefined}
            />
          </label>

          <div className="login-card__row">
            <label className="checkbox">
              <input type="checkbox" checked={remember} onChange={(e) => setRemember(e.target.checked)} />
              Remember me
            </label>
          </div>

          {errorText && (
            <p id={errorId} className="form-error" role="alert">
              {errorText}
            </p>
          )}

          <button type="submit" className="button button--dark button--block" disabled={login.isPending || waiting}>
            {login.isPending ? 'Signing in…' : waiting ? `Try again ${waitText(secsLeft ?? 0)}` : 'Sign in'}
          </button>

        </form>
      </main>

      <footer className="login__footer">
        <span>{sys ? `Hangar ${versionText(sys.version)}` : 'Hangar'}</span>
        <a href={DOCS.guides} target="_blank" rel="noreferrer">
          Documentation
        </a>
        <a href={API_URL}>API reference</a>
      </footer>
    </div>
  );
}
