import { useEffect, useId, useRef, useState, type FormEvent, type ReactNode } from 'react';
import { ApiRequestError } from '../api/client';
import { useAuthProviders, useLogin } from '../api/hooks';
import type { Session } from '../api/types';
import { useNow } from '../lib/useNow';

function waitText(secs: number): string {
  if (secs <= 0) return 'now';
  if (secs < 60) return `in ${secs} s`;
  return `in ${Math.ceil(secs / 60)} min`;
}

function minutes(secs: number): string {
  const m = Math.max(1, Math.round(secs / 60));
  return `${m}-minute`;
}

/** An API message as a sentence: "too many x; try again later" -> "Too many x. Try again later." */
function sentence(message: string | undefined): string | null {
  const m = message?.trim();
  if (!m) return null;
  const text = m.replace(/;\s+(\w)/g, (_, c: string) => `. ${c.toUpperCase()}`);
  return `${text.charAt(0).toUpperCase()}${text.slice(1)}${/[.!?]$/.test(text) ? '' : '.'}`;
}

/** Shown from this many attempts left: earlier warnings would only be noise. */
const WARN_FROM = 2;

/**
 * What went wrong with a sign-in. The text stays the same while a lockout or
 * rate limit runs (it is announced once); the submit button carries the live
 * countdown.
 */
function loginError(err: unknown, expired: boolean): string {
  if (err instanceof ApiRequestError) {
    const wait = err.retryAfter;
    if (err.status === 401) {
      const left = err.body?.details?.attempts_left;
      const lockSecs = err.body?.details?.lockout_seconds;
      if (typeof left === 'number' && left > 0 && left <= WARN_FROM) {
        const lock = typeof lockSecs === 'number' && lockSecs > 0 ? `a ${minutes(lockSecs)} lock` : 'a lock';
        return `Wrong username or password. ${left === 1 ? '1 attempt' : `${left} attempts`} left before ${lock}.`;
      }
      return 'Wrong username or password.';
    }
    if (err.status === 423) {
      if (expired) return 'The lock has expired. You can try again.';
      // The server locks this username for this address only: never claim the whole machine or "this account".
      const length = wait ? ` for ${waitText(wait).replace(/^in /, '')}` : '';
      return `This sign-in is locked${length} after too many failed attempts. An administrator can unlock it sooner.`;
    }
    if (err.status === 429) {
      if (expired) return 'You can try again now.';
      return `Too many sign-in attempts from this address. ${wait ? `Try again ${waitText(wait)}.` : 'Wait a moment and try again.'}`;
    }
    if (err.status === 403) {
      if (err.body?.code === 'forbidden' && err.body.message?.includes('cross-origin'))
        return 'The server rejected this sign-in request. Open Hangar from its own address and try again.';
      return sentence(err.body?.message) ?? 'You are not allowed to sign in.';
    }
    return sentence(err.body?.message) ?? 'Sign-in failed. Please try again.';
  }
  return 'Cannot reach the server. Check your connection and try again.';
}

type Missing = 'username' | 'password' | 'both' | null;

const MISSING_TEXT: Record<Exclude<Missing, null>, string> = {
  username: 'Enter your username.',
  password: 'Enter your password.',
  both: 'Enter your username and password.',
};

interface SignInFormProps {
  className?: string;
  'aria-labelledby'?: string;
  /** Above the sign-in options (title, logo). */
  header?: ReactNode;
  /** Shown when nothing else is wrong (e.g. why the user is here). */
  notice?: ReactNode;
  /** Below the submit button. */
  footer?: ReactNode;
  /** Pre-filled name; fixed, it is shown and cannot be changed. */
  username?: string;
  fixedUsername?: boolean;
  /** Single sign-on opens in a new tab, so the page behind keeps its state. */
  ssoInNewTab?: boolean;
  /** The page's data stays (same user signing in again); see useLogin. */
  keepCache?: boolean;
  /** An error from outside the form, e.g. `?error=` of the single sign-on callback. */
  externalError?: string | null;
  onSignedIn: (session: Session, password: string) => void;
}

/**
 * The sign-in options and the local or directory password form, shared by
 * the sign-in page and the "sign in again" dialog.
 */
export function SignInForm(props: SignInFormProps) {
  const { header, notice, footer, fixedUsername, ssoInNewTab, keepCache, externalError, onSignedIn } = props;
  const providers = useAuthProviders();
  const login = useLogin({ keepCache });
  const p = providers.data;
  const external = Boolean(p?.oidc || p?.ldap.enabled || (p?.saml.enabled && p.saml.start_url));
  const sso = ssoInNewTab ? { target: '_blank', rel: 'noreferrer' } : {};

  const [realm, setRealm] = useState<'local' | 'ldap'>('local');
  const [username, setUsername] = useState(props.username ?? '');
  const [password, setPassword] = useState('');
  const [remember, setRemember] = useState(false);
  const [missing, setMissing] = useState<Missing>(null);
  const errorId = useId();
  const userRef = useRef<HTMLInputElement>(null);
  const passRef = useRef<HTMLInputElement>(null);

  // A lockout (423) or rate limit (429) holds the name it was for: another
  // name may sign in at once (the server locks a name per address).
  const [retry, setRetry] = useState<{ at: number; secs: number; name: string; status: number; error: unknown } | null>(null);
  const name = username.trim().toLowerCase();
  const held = retry && (retry.status === 429 || retry.name === name) ? retry : null;
  // The clock runs only while a countdown is on screen.
  const now = useNow(held ? 1000 : null, held?.at).getTime();
  // The clock ticks once a second, so it can lag the moment the wait started: clamp.
  const secsLeft = held === null ? null : Math.min(held.secs, Math.max(0, Math.ceil((held.at - now) / 1000)));
  const waiting = secsLeft !== null && secsLeft > 0;

  useEffect(() => {
    if (fixedUsername) passRef.current?.focus();
  }, [fixedUsername]);

  const onSubmit = (e: FormEvent) => {
    e.preventDefault();
    if (waiting) return;
    const lack: Missing = !username.trim() && !password ? 'both' : !username.trim() ? 'username' : !password ? 'password' : null;
    setMissing(lack);
    if (lack) {
      (lack === 'password' ? passRef : userRef).current?.focus();
      return;
    }
    const typed = password;
    login.mutate(
      { username: username.trim(), password, remember, realm },
      {
        onSuccess: (session) => {
          setRetry(null);
          onSignedIn(session, typed);
        },
        onError: (err) => {
          const limited = err instanceof ApiRequestError && (err.status === 423 || err.status === 429);
          const wait = limited ? err.retryAfter : null;
          // A wrong password for another name leaves an earlier lock where it was.
          setRetry((prev) =>
            wait === null
              ? prev && prev.status === 423 && prev.name !== name
                ? prev
                : null
              : { at: Date.now() + wait * 1000, secs: wait, name, status: (err as ApiRequestError).status, error: err },
          );
          if (err instanceof ApiRequestError && err.status === 401) {
            // The wrong password is of no use: clear it for the next try.
            setPassword('');
            passRef.current?.focus();
          }
        },
      },
    );
  };

  // A lock message follows the name it was for, also after editing cleared the last answer.
  const shown = held ? held.error : login.isError ? login.error : null;
  const errorText = missing ? MISSING_TEXT[missing] : shown ? loginError(shown, held !== null && secsLeft === 0) : (externalError ?? null);
  const userInvalid = missing === 'username' || missing === 'both' || (!missing && !!shown && !fixedUsername);
  const passInvalid = missing === 'password' || missing === 'both' || (!missing && !!shown);

  return (
    <form className={props.className} onSubmit={onSubmit} aria-labelledby={props['aria-labelledby']} noValidate>
      {header}

      {p?.oidc && (
        <a className="button button--primary button--block" href={p.oidc.start_url} {...sso}>
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
            <a className="button button--secondary" href={p.saml.start_url} {...sso}>
              SAML
            </a>
          )}
        </div>
      )}

      {external && <div className="divider">{realm === 'ldap' ? 'with your directory account' : 'or with a local account'}</div>}

      <label className="field">
        <span className="field__label">{realm === 'ldap' ? 'Directory username' : 'Username'}</span>
        <input
          ref={userRef}
          className="input"
          type="text"
          name="username"
          autoComplete="username"
          autoCapitalize="off"
          spellCheck={false}
          required
          readOnly={fixedUsername}
          autoFocus={!fixedUsername}
          value={username}
          onChange={(e) => {
            setUsername(e.target.value);
            if (missing === 'username' || missing === 'both') setMissing(e.target.value.trim() ? (missing === 'both' ? 'password' : null) : missing);
            // A new name gets a fresh start; the lock stays with the name it was for.
            if (login.isError) login.reset();
          }}
          aria-invalid={userInvalid || undefined}
          aria-describedby={errorText ? errorId : undefined}
        />
      </label>
      <label className="field">
        <span className="field__label">Password</span>
        <input
          ref={passRef}
          className="input"
          type="password"
          name="password"
          autoComplete="current-password"
          required
          value={password}
          onChange={(e) => {
            setPassword(e.target.value);
            if (e.target.value && (missing === 'password' || missing === 'both')) setMissing(missing === 'both' ? 'username' : null);
          }}
          aria-invalid={passInvalid || undefined}
          aria-describedby={errorText ? errorId : undefined}
        />
      </label>

      <div className="login-card__row">
        <label className="checkbox">
          <input type="checkbox" checked={remember} onChange={(e) => setRemember(e.target.checked)} />
          Remember me
        </label>
      </div>

      {errorText ? (
        <p id={errorId} className="form-error" role="alert">
          {errorText}
        </p>
      ) : (
        notice
      )}

      <button type="submit" className="button button--dark button--block" disabled={login.isPending || waiting}>
        {login.isPending ? 'Signing in…' : waiting ? `Try again ${waitText(secsLeft ?? 0)}` : 'Sign in'}
      </button>
      {footer}
    </form>
  );
}
