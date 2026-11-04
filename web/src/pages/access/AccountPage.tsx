import { useId, useState, type FormEvent } from 'react';
import { useNavigate } from '@tanstack/react-router';
import { useSession } from '../../api/hooks';
import { IconPlus } from '../../components/icons';
import { PageHeader } from '../../components/PageHeader';
import { Panel } from '../../components/Panel';
import { Pill } from '../../components/Pill';
import { useChangePassword, useEndMyOtherSessions, useEndMySession, useMySessions, useTokens, useUpdateProfile, type SessionInfo } from './api';
import { PermissionsReport, SessionList } from './members';
import { expiryText, roleTone, SOURCE_LABEL } from './model';
import { CreateTokenDialog, TokenTable } from './tokens';
import { ConfirmDialog, Field, FormError, Notice } from './ui';
import './access.css';

/** My account: profile, password, personal access tokens, sessions and what I may do. */
export function AccountPage() {
  const session = useSession();
  const s = session.data;
  if (!s) return null;
  const local = (s.user.source ?? 'local') === 'local' && s.user.kind !== 'service_account';
  return (
    <div className="screen">
      <PageHeader title="My account" description="Your profile, password, access tokens and signed-in devices." />
      {s.token_warnings && s.token_warnings.length > 0 && <TokenWarnings />}
      <div className="account-grid">
        <ProfilePanel editable={local && s.auth_method !== 'token'} />
        {local && s.auth_method !== 'token' && <PasswordPanel />}
      </div>
      <MyTokens />
      {s.auth_method !== 'token' && <MySessions />}
      <Panel title="What you can do" titleId="my-permissions-title">
        <PermissionsReport id={s.user.id} />
      </Panel>
    </div>
  );
}

/** Tokens that expire soon (the session payload lists them). */
export function TokenWarnings() {
  const session = useSession();
  const warnings = session.data?.token_warnings ?? [];
  if (warnings.length === 0) return null;
  return (
    <div className="warn-note" role="status">
      <strong>
        {warnings.length} token{warnings.length === 1 ? '' : 's'} expire{warnings.length === 1 ? 's' : ''} soon:
      </strong>{' '}
      {warnings.map((t, i) => (
        <span key={t.id}>
          {i > 0 && ', '}“{t.name}”{t.owner !== session.data?.user.username ? ` (${t.owner})` : ''} {expiryText(t.expires_at)}
        </span>
      ))}
      . Create a replacement before it stops working.
    </div>
  );
}

function ProfilePanel({ editable }: { editable: boolean }) {
  const s = useSession().data!;
  const [name, setName] = useState(s.user.display_name);
  const [email, setEmail] = useState(s.user.email);
  const [saved, setSaved] = useState(false);
  const update = useUpdateProfile();
  const ids = { name: useId(), email: useId() };
  const submit = (e: FormEvent) => {
    e.preventDefault();
    setSaved(false);
    update.mutate({ display_name: name, email }, { onSuccess: () => setSaved(true) });
  };
  return (
    <Panel title="Profile" titleId="profile-title">
      <form className="dialog-form" onSubmit={submit}>
        <dl className="summary__list">
          <dt>Username</dt>
          <dd className="mono">{s.user.username}</dd>
          <dt>Signs in with</dt>
          <dd>{s.auth_method === 'token' ? 'Access token' : (SOURCE_LABEL[s.user.source ?? 'local'] ?? s.user.source)}</dd>
          <dt>Roles</dt>
          <dd>
            {s.user.roles?.length ? (
              <span className="pill-row">
                {s.user.roles.map((r) => (
                  <Pill key={r} tone={roleTone(r)}>
                    {r}
                  </Pill>
                ))}
              </span>
            ) : (
              'None'
            )}
          </dd>
        </dl>
        <Field id={ids.name} label="Display name">
          <input className="input" value={name} onChange={(e) => setName(e.target.value)} readOnly={!editable} maxLength={200} />
        </Field>
        <Field id={ids.email} label="Email">
          <input type="email" className="input" value={email} onChange={(e) => setEmail(e.target.value)} readOnly={!editable} />
        </Field>
        {!editable && <p className="meta">Your profile is managed by your sign-in provider.</p>}
        <FormError error={update.error} />
        {saved && <Notice message="Profile saved." />}
        {editable && (
          <div className="dialog-actions">
            <button type="submit" className="button button--primary" disabled={update.isPending}>
              Save profile
            </button>
          </div>
        )}
      </form>
    </Panel>
  );
}

function PasswordPanel() {
  const [current, setCurrent] = useState('');
  const [next, setNext] = useState('');
  const [confirm, setConfirm] = useState('');
  const [submitted, setSubmitted] = useState(false);
  const [done, setDone] = useState(false);
  const change = useChangePassword();
  const ids = { cur: useId(), next: useId(), confirm: useId() };
  const errors = {
    next: next.length < 12 ? 'At least 12 characters.' : next === current ? 'Choose a password different from the current one.' : null,
    confirm: confirm !== next ? 'The passwords do not match.' : null,
  };
  const submit = (e: FormEvent) => {
    e.preventDefault();
    setSubmitted(true);
    setDone(false);
    if (!current || errors.next || errors.confirm) return;
    change.mutate(
      { current_password: current, new_password: next },
      {
        onSuccess: () => {
          setDone(true);
          setSubmitted(false);
          setCurrent('');
          setNext('');
          setConfirm('');
        },
      },
    );
  };
  return (
    <Panel title="Password" titleId="password-title">
      <form className="dialog-form" onSubmit={submit} noValidate>
        <p className="meta">Changing it signs you out on every other device. Access tokens keep working.</p>
        <Field id={ids.cur} label="Current password" error={submitted && !current && 'Enter your current password.'}>
          <input type="password" className="input" value={current} onChange={(e) => setCurrent(e.target.value)} autoComplete="current-password" />
        </Field>
        <Field id={ids.next} label="New password" error={submitted && errors.next} hint="A long passphrase is best.">
          <input type="password" className="input" value={next} onChange={(e) => setNext(e.target.value)} autoComplete="new-password" />
        </Field>
        <Field id={ids.confirm} label="Repeat new password" error={submitted && errors.confirm}>
          <input type="password" className="input" value={confirm} onChange={(e) => setConfirm(e.target.value)} autoComplete="new-password" />
        </Field>
        <FormError error={change.error} />
        {done && <Notice message="Password changed. Your other sessions were signed out." />}
        <div className="dialog-actions">
          <button type="submit" className="button button--primary" disabled={change.isPending}>
            {change.isPending ? 'Changing…' : 'Change password'}
          </button>
        </div>
      </form>
    </Panel>
  );
}

function MyTokens() {
  const tokens = useTokens({ limit: 100 });
  const [creating, setCreating] = useState(false);
  return (
    <Panel
      flush
      title="Personal access tokens"
      titleId="my-tokens-title"
      action={
        <button type="button" className="button button--secondary button--sm" onClick={() => setCreating(true)}>
          <IconPlus size={14} /> New token
        </button>
      }
    >
      <p className="meta panel-intro">For CLIs and scripts: use a token as the password with docker, npm, Maven, pip and NuGet, or as a Bearer token for the API.</p>
      {tokens.isError ? (
        <FormError error={tokens.error} />
      ) : (
        <TokenTable caption="Your access tokens" tokens={tokens.data?.items ?? []} busy={tokens.isFetching} empty={tokens.isPending ? 'Loading…' : 'You have no tokens yet.'} />
      )}
      {creating && <CreateTokenDialog onClose={() => setCreating(false)} />}
    </Panel>
  );
}

function MySessions() {
  const sessions = useMySessions();
  const endOne = useEndMySession();
  const endOthers = useEndMyOtherSessions();
  const navigate = useNavigate();
  const [ending, setEnding] = useState<SessionInfo | 'others' | null>(null);
  const [notice, setNotice] = useState('');
  const others = (sessions.data?.items ?? []).filter((s) => !s.current).length;
  return (
    <Panel
      title="Signed-in devices"
      titleId="my-sessions-title"
      action={
        <button type="button" className="button button--secondary button--sm" disabled={others === 0} onClick={() => setEnding('others')}>
          Sign out everywhere else
        </button>
      }
    >
      {notice && <Notice message={notice} />}
      {sessions.isError ? (
        <FormError error={sessions.error} />
      ) : sessions.data ? (
        <SessionList sessions={sessions.data.items} onEnd={setEnding} busy={endOne.isPending} />
      ) : (
        <p className="meta">Loading…</p>
      )}
      {ending === 'others' && (
        <ConfirmDialog
          title="Sign out everywhere else?"
          confirmLabel="Sign out other devices"
          onConfirm={() => endOthers.mutateAsync().then((r) => setNotice(`${r.ended} session${r.ended === 1 ? '' : 's'} ended.`))}
          onClose={() => setEnding(null)}
        >
          Every other browser signed in as you is signed out. This one stays signed in.
        </ConfirmDialog>
      )}
      {ending && ending !== 'others' && (
        <ConfirmDialog
          title={ending.current ? 'Sign out of this session?' : 'End this session?'}
          confirmLabel={ending.current ? 'Sign out' : 'End session'}
          onConfirm={async () => {
            await endOne.mutateAsync(ending.id);
            if (ending.current) void navigate({ to: '/login', replace: true });
            else setNotice('Session ended.');
          }}
          onClose={() => setEnding(null)}
        >
          {ending.current ? 'You will need to sign in again.' : `The browser at ${ending.ip ?? 'an unknown address'} is signed out at once.`}
        </ConfirmDialog>
      )}
    </Panel>
  );
}
