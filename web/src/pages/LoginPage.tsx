import { getRouteApi, useRouter } from '@tanstack/react-router';
import { useSystemStatus } from '../api/hooks';
import type { Session } from '../api/types';
import { IconLogo } from '../components/icons';
import { SignInForm } from '../components/SignInForm';
import { DOCS } from '../lib/docs';
import { instanceName, versionText } from '../lib/format';
import { holdSignInPassword } from '../lib/signInPassword';

const route = getRouteApi('/login');

const API_URL = '/api-docs';

const STATE_TONE = { ok: 'ok', degraded: 'warn', down: 'bad' } as const;

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

export function LoginPage() {
  const { redirect, error: ssoError, reason } = route.useSearch();
  const router = useRouter();
  const status = useSystemStatus();
  const sys = status.data;
  const instance = instanceName(sys?.instance_name);

  const signedIn = (session: Session, password: string) => {
    // Only follow same-origin, in-app redirects (path plus query string).
    const target = redirect && redirect.startsWith('/') && !redirect.startsWith('//') ? redirect : '/';
    // The forced change next asks for the old password: keep what was just typed (in memory only).
    if (session.password_change_required) holdSignInPassword(password);
    router.history.push(target);
  };

  const tone = sys ? STATE_TONE[sys.state] : null;

  return (
    <div className="login wall wall--center">
      {sys && tone && (
        <header className="login__head">
          <span className="login__status">
            <span className={`status__dot dot-${tone}`} aria-hidden="true" />
            {sys.message}
          </span>
        </header>
      )}
      <main className="login__main">
        <SignInForm
          className="login-card glass"
          aria-labelledby="login-title"
          externalError={ssoError ? (SSO_ERRORS[ssoError] ?? 'Single sign-on failed. Please try again.') : null}
          notice={
            reason === 'expired' ? (
              <p className="login-card__note" role="status">
                Your session ended. Sign in again to continue.
              </p>
            ) : undefined
          }
          onSignedIn={signedIn}
          header={
            <div className="login-card__intro">
              <span className="brand__mark brand__mark--lg login-card__mark" aria-hidden="true">
                <IconLogo size={20} />
              </span>
              <h1 id="login-title" className="login-card__title">
                Welcome back
              </h1>
              <p className="login-card__sub">Sign in to {instance}</p>
            </div>
          }
          footer={
            <div className="login-card__hints">
              <p>Forgot your password? Ask an administrator to reset it.</p>
              <p>For CI jobs and package clients, use an access token from My account instead of a password.</p>
            </div>
          }
        />
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
