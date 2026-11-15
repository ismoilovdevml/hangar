import { useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { useNavigate, useRouterState } from '@tanstack/react-router';
import { apiFetch } from '../api/client';
import { queryKeys, useAuthProviders } from '../api/hooks';
import type { Session } from '../api/types';
import { Dialog } from '../pages/access/ui';
import { endReauth, useReauth } from '../lib/reauth';
import { toast } from '../lib/toast';
import { SignInForm } from './SignInForm';

/**
 * "Sign in again" over the page whose session ended, so nothing typed into
 * it is lost. A write that failed is not repeated: the user saves again.
 */
export function ReauthDialog() {
  const username = useReauth();
  return username ? <Reauth key={username} username={username} /> : null;
}

function Reauth({ username }: { username: string }) {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const href = useRouterState({ select: (s) => s.location.href });
  const providers = useAuthProviders();
  const sso = Boolean(providers.data?.oidc || (providers.data?.saml.enabled && providers.data.saml.start_url));
  const [checking, setChecking] = useState(false);
  const [notYet, setNotYet] = useState(false);

  const resume = (session: Session) => {
    qc.setQueryData(queryKeys.session, session);
    endReauth();
    // What failed while signed out loads again; nothing is sent twice.
    void qc.invalidateQueries({ predicate: (q) => q.queryKey[0] !== 'session' });
    toast('Signed in again. Anything that was not saved is still on the page: save it again.');
  };

  const someoneElse = () => {
    endReauth();
    qc.clear();
    void navigate({ to: '/login', search: { redirect: href, reason: 'expired' }, replace: true });
  };

  // Single sign-on runs in another tab; this checks whether it has finished.
  const checkSession = async () => {
    setChecking(true);
    setNotYet(false);
    try {
      const s = await apiFetch<Session>('/auth/session', { allowUnauthorized: true });
      if (s.user.username.toLowerCase() === username.toLowerCase()) resume(s);
      else someoneElse();
    } catch {
      setNotYet(true);
    } finally {
      setChecking(false);
    }
  };

  return (
    <Dialog
      title="Your session ended"
      description="Sign in again to carry on where you were. Nothing on this page is lost, but changes you tried to save were not saved."
      onClose={() => {}}
      dismissible={false}
    >
      <SignInForm
        className="dialog-form"
        username={username}
        fixedUsername
        keepCache
        ssoInNewTab
        onSignedIn={(s) => resume(s)}
        footer={
          <div className="reauth__more">
            {sso && (
              <p className="meta">
                Single sign-on opens in a new tab. When you have signed in there,{' '}
                <button type="button" className="link-button" onClick={() => void checkSession()} disabled={checking}>
                  {checking ? 'checking…' : 'continue here'}
                </button>
                .{notYet && ' You are not signed in yet.'}
              </p>
            )}
            <p className="meta">
              Not {username}?{' '}
              <button type="button" className="link-button" onClick={someoneElse}>
                Sign in as someone else
              </button>{' '}
              (this page is not kept).
            </p>
          </div>
        }
      />
    </Dialog>
  );
}
