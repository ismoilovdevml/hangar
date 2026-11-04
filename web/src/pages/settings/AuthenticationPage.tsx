import { useId, useState, type FormEvent, type ReactNode } from 'react';
import { useCanSystem } from '../../api/ops';
import { CopyButton } from '../../components/CopyButton';
import { ErrorNote } from '../../components/ErrorNote';
import { PageHeader } from '../../components/PageHeader';
import { Panel } from '../../components/Panel';
import { StatusDot } from '../../components/Pill';
import { formatUtc } from '../../lib/tasks';
import { useRoles } from '../access/api';
import { Select } from '../../components/Select';
import { Field, FileBadge, ListInput, RoleMappingTable, RolesInput, SecretInput, TestResultView, Toggle } from './controls';
import { ldapForm, ldapInput, oidcForm, oidcInput, validateLdap, validateOidc, type Errors, type LdapForm, type OidcForm } from './model';
import {
  errorMessage,
  fieldErrorsOf,
  useIdentityProviders,
  useLdapSettings,
  useOidcSettings,
  useSaveLdap,
  useSaveOidc,
  useTestLdap,
  useTestOidc,
  type IdentityProvider,
  type LDAPSettings,
  type OIDCSettings,
} from './settingsApi';
import { NoPermission } from './NoPermission';
import './settings.css';

export function AuthenticationPage() {
  const canRead = useCanSystem('settings', 'read');
  const canEdit = useCanSystem('settings', 'admin');
  if (!canRead) return <NoPermission what="sign-in settings" />;
  return (
    <div className="screen settings-screen">
      <PageHeader
        eyebrow="Settings"
        title="Authentication"
        description="Let people sign in with your identity provider or directory. Saved changes apply at once on every node, without a restart."
      />
      <ProviderStatus />
      <OidcPanel canEdit={canEdit} />
      <LdapPanel canEdit={canEdit} />
    </div>
  );
}

const STATE_TONE = { ok: 'ok', error: 'bad', disabled: 'neutral' } as const;
const PROVIDER_LABEL: Record<string, string> = { oidc: 'Single sign-on (OIDC)', ldap: 'LDAP directory' };

function ProviderStatus() {
  const q = useIdentityProviders();
  if (q.isError) return <ErrorNote what="the sign-in status" onRetry={() => void q.refetch()} />;
  const items = q.data?.items ?? [];
  return (
    <Panel className="provider-status" aria-label="Sign-in status" aria-busy={q.isPending || undefined}>
      {q.isPending ? (
        <p className="meta">Checking the sign-in providers…</p>
      ) : (
        <ul className="provider-status__list">
          <li>
            <StatusDot tone="ok" label={<strong>Local accounts</strong>} /> <span className="meta">always on</span>
          </li>
          {items.map((p: IdentityProvider) => (
            <li key={p.name}>
              <StatusDot tone={STATE_TONE[p.state]} label={<strong>{PROVIDER_LABEL[p.kind] ?? p.name}</strong>} />{' '}
              <span className="meta">
                {p.state === 'disabled' ? 'off' : p.state === 'ok' ? 'working' : `failing: ${p.error ?? 'unknown error'}`}
                {p.last_sync_at && ` · last request ${formatUtc(p.last_sync_at)}`}
              </span>
            </li>
          ))}
        </ul>
      )}
    </Panel>
  );
}

/** Names of the existing roles, for the role pickers (empty when roles cannot be listed). */
function useRoleNames(): string[] {
  const roles = useRoles(useCanSystem('users', 'read'));
  return (roles.data?.items ?? []).map((r) => r.name);
}

function SourceNote({ source, what, file, updatedAt, updatedBy }: { source: string; what: string; file: string; updatedAt?: string | null; updatedBy?: string }) {
  if (source === 'file') {
    return (
      <p className="note settings-file-note">
        {what} is set in the configuration file (<code>{file}</code> in hangar.yaml or the HANGAR_* environment) and is shown read-only. Change it there and
        restart Hangar.
      </p>
    );
  }
  if (source === 'database' && updatedAt) {
    return (
      <p className="meta">
        Last saved {formatUtc(updatedAt)}
        {updatedBy && ` by ${updatedBy}`}.
      </p>
    );
  }
  return null;
}

/** Field label with the "set in configuration file" badge for file-managed fields. */
function label(text: ReactNode, field: string, fileFields: readonly string[]) {
  return (
    <>
      {text}
      {fileFields.includes(field) && (
        <>
          {' '}
          <FileBadge />
        </>
      )}
    </>
  );
}

function SaveBar({ busy, canEdit, canTest, onTest, testing, saved, error, children }: {
  busy: boolean;
  canEdit: boolean;
  canTest: boolean;
  onTest: () => void;
  testing: boolean;
  saved: string | null;
  error: unknown;
  children?: ReactNode;
}) {
  return (
    <div className="settings-actions">
      {canTest && (
        <>
          {children}
          <button type="button" className="button button--secondary" onClick={onTest} disabled={testing}>
            {testing ? 'Testing…' : 'Test'}
          </button>
        </>
      )}
      {canEdit && (
        <button type="submit" className="button button--primary" disabled={busy}>
          {busy ? 'Saving…' : 'Save'}
        </button>
      )}
      {saved && (
        <p className="form-success settings-actions__msg" role="status">
          {saved}
        </p>
      )}
      {!!error && (
        <p className="form-error settings-actions__msg" role="alert">
          {errorMessage(error)}
        </p>
      )}
    </div>
  );
}

/* ---------------------------------------------------------------- OIDC */

function OidcPanel({ canEdit }: { canEdit: boolean }) {
  const q = useOidcSettings();
  return (
    <Panel title="Single sign-on (OIDC)" titleId="oidc-title" aria-busy={q.isPending || undefined}>
      {q.isError ? (
        <ErrorNote what="the OIDC settings" onRetry={() => void q.refetch()} />
      ) : q.data ? (
        <OidcEditor key={q.data.source} data={q.data} canEdit={canEdit} />
      ) : (
        <p className="meta">Loading…</p>
      )}
    </Panel>
  );
}

function OidcEditor({ data, canEdit }: { data: OIDCSettings; canEdit: boolean }) {
  const id = useId();
  const roles = useRoleNames();
  const [form, setForm] = useState<OidcForm>(() => oidcForm(data));
  const [errors, setErrors] = useState<Errors>({});
  const [saved, setSaved] = useState<string | null>(null);
  const save = useSaveOidc();
  const test = useTestOidc();
  const file = data.source === 'file';
  const locked = file || !canEdit;
  const ff = data.file_fields;
  const set = <K extends keyof OidcForm>(k: K, v: OidcForm[K]) => {
    setForm((f) => ({ ...f, [k]: v }));
    setSaved(null);
  };
  const err = (k: string) => errors[k];

  const submit = (e: FormEvent) => {
    e.preventDefault();
    const found = validateOidc(form);
    setErrors(found);
    setSaved(null);
    if (Object.keys(found).length) return;
    save.mutate(oidcInput(form), {
      onSuccess: (s) => {
        // The stored secret is now the one to keep.
        setForm((f) => ({ ...f, client_secret: '', clear_client_secret: false }));
        setSaved(s.enabled ? 'Saved. Single sign-on is on for every node.' : 'Saved. Single sign-on is off.');
      },
      onError: (e2) => setErrors(fieldErrorsOf(e2)),
    });
  };
  const runTest = () => {
    const found = validateOidc({ ...form, enabled: true });
    if (!file && Object.keys(found).length) {
      setErrors(found);
      return;
    }
    test.mutate(file ? undefined : oidcInput(form), { onError: (e2) => setErrors(fieldErrorsOf(e2)) });
  };

  return (
    <form className="settings-form" onSubmit={submit} noValidate aria-labelledby="oidc-title">
      <SourceNote source={data.source} what="Single sign-on" file="auth.oidc" updatedAt={data.updated_at} updatedBy={data.updated_by} />
      <div className="redirect-uri">
        <span className="field__label">Redirect URI to register at your identity provider</span>
        <span className="redirect-uri__value">
          <code>{data.redirect_uri}</code>
          <CopyButton text={data.redirect_uri} label="Copy the redirect URI" />
        </span>
      </div>
      <fieldset className="settings-fieldset" disabled={locked}>
        <legend className="visually-hidden">OIDC settings</legend>
        <Toggle checked={form.enabled} onChange={(v) => set('enabled', v)} label={label('Allow sign-in with single sign-on', 'enabled', ff)} disabled={locked} />
        <div className="form-grid">
          <Field id={`${id}-issuer`} label={label('Issuer URL', 'issuer_url', ff)} error={err('issuer_url')} hint="e.g. https://sso.example.com/realms/acme">
            {(a) => <input {...a} className="input input--mono" type="url" autoComplete="off" spellCheck={false} value={form.issuer_url} onChange={(e) => set('issuer_url', e.target.value)} />}
          </Field>
          <Field id={`${id}-client`} label={label('Client ID', 'client_id', ff)} error={err('client_id')}>
            {(a) => <input {...a} className="input input--mono" autoComplete="off" spellCheck={false} value={form.client_id} onChange={(e) => set('client_id', e.target.value)} />}
          </Field>
          <Field
            id={`${id}-secret`}
            label={label('Client secret', 'client_secret', ff)}
            error={err('client_secret')}
            hint={data.client_secret_configured ? 'A secret is stored. Type a new one to replace it.' : 'Leave empty for a public client (PKCE only).'}
          >
            {(a) => (
              <SecretInput
                id={a.id}
                a11y={a}
                configured={data.client_secret_configured}
                value={form.client_secret}
                onChange={(v) => set('client_secret', v)}
                cleared={form.clear_client_secret}
                onClear={(v) => set('clear_client_secret', v)}
                disabled={locked}
              />
            )}
          </Field>
          <Field id={`${id}-auth`} label={label('Client authentication', 'client_auth', ff)} error={err('client_auth')}>
            {(a) => (
              <Select
                id={a.id}
                aria-describedby={a['aria-describedby']}
                value={form.client_auth}
                onChange={(v) => set('client_auth', v)}
                disabled={locked}
                options={[
                  { value: 'basic', label: 'HTTP Basic (client_secret_basic)' },
                  { value: 'post', label: 'In the request body (client_secret_post)' },
                ]}
              />
            )}
          </Field>
          <Field id={`${id}-name`} label={label('Button label', 'display_name', ff)} error={err('display_name')} hint='Shown as "Continue with …" on the sign-in page'>
            {(a) => <input {...a} className="input" maxLength={64} value={form.display_name} onChange={(e) => set('display_name', e.target.value)} />}
          </Field>
          <Field id={`${id}-scopes`} label={label('Scopes', 'scopes', ff)} error={err('scopes')} hint="openid is required">
            {(a) => (
              <ListInput id={a.id} describedBy={a['aria-describedby']} invalid={!!a['aria-invalid']} itemLabel="scope" values={form.scopes} onChange={(v) => set('scopes', v)} disabled={locked} placeholder="groups" />
            )}
          </Field>
        </div>

        <h3 className="section-label">Claims</h3>
        <div className="form-grid form-grid--4">
          {(
            [
              ['username_claim', 'Username'],
              ['email_claim', 'Email'],
              ['name_claim', 'Display name'],
              ['groups_claim', 'Groups'],
            ] as const
          ).map(([k, text]) => (
            <Field key={k} id={`${id}-${k}`} label={label(text, k, ff)} error={err(k)}>
              {(a) => <input {...a} className="input input--mono" autoComplete="off" spellCheck={false} value={form[k]} onChange={(e) => set(k, e.target.value)} />}
            </Field>
          ))}
        </div>
        <p className="meta">A dotted name reaches into nested claims, e.g. realm_access.roles.</p>

        <MappingSection
          id={id}
          ff={ff}
          rows={form.group_roles}
          defaultRoles={form.default_roles}
          allowedGroups={form.allowed_groups}
          autoCreate={form.auto_create}
          roles={roles}
          errors={errors}
          locked={locked}
          groupPlaceholder="/devops"
          onRows={(v) => set('group_roles', v)}
          onDefaultRoles={(v) => set('default_roles', v)}
          onAllowedGroups={(v) => set('allowed_groups', v)}
          onAutoCreate={(v) => set('auto_create', v)}
        />

        <details className="advanced" open={!!form.ca_pem || !!data.ca_file}>
          <summary className="advanced__summary">Trusted certificates</summary>
          {data.ca_file && (
            <p className="meta">
              The configuration file trusts <code>{data.ca_file}</code>.
            </p>
          )}
          <Field id={`${id}-ca`} label={label('CA certificates', 'ca_pem', ff)} error={err('ca_pem')} optional hint="PEM; for a provider with a private certificate authority">
            {(a) => <textarea {...a} className="input input--mono textarea" rows={4} spellCheck={false} placeholder="-----BEGIN CERTIFICATE-----" value={form.ca_pem} onChange={(e) => set('ca_pem', e.target.value)} />}
          </Field>
        </details>
      </fieldset>

      {test.data && <TestResultView result={test.data} />}
      {test.isError && Object.keys(fieldErrorsOf(test.error)).length === 0 && (
        <p className="form-error" role="alert">
          {errorMessage(test.error)}
        </p>
      )}
      <SaveBar busy={save.isPending} canEdit={!locked} canTest={canEdit} onTest={runTest} testing={test.isPending} saved={saved} error={save.error} />
    </form>
  );
}

/* ---------------------------------------------------------------- mapping */

function MappingSection(p: {
  id: string;
  ff: readonly string[];
  rows: OidcForm['group_roles'];
  defaultRoles: string[];
  allowedGroups: string[];
  autoCreate: boolean;
  roles: string[];
  errors: Errors;
  locked: boolean;
  groupPlaceholder: string;
  onRows: (v: OidcForm['group_roles']) => void;
  onDefaultRoles: (v: string[]) => void;
  onAllowedGroups: (v: string[]) => void;
  onAutoCreate: (v: boolean) => void;
}) {
  return (
    <section className="settings-subsection" aria-label="Roles and access">
      <h3 className="section-label">
        {label('Group → role mapping', 'group_roles', p.ff)}
      </h3>
      <p className="meta">Members of a group get its roles at every sign-in. Group names match case-insensitively, with or without a leading "/".</p>
      <RoleMappingTable rows={p.rows} onChange={p.onRows} roles={p.roles} errors={p.errors} disabled={p.locked} groupPlaceholder={p.groupPlaceholder} />
      {p.errors.group_roles && <span className="field__error">{p.errors.group_roles}</span>}
      <div className="form-grid">
        <Field id={`${p.id}-defroles`} label={label('Default roles', 'default_roles', p.ff)} error={p.errors.default_roles} hint="Granted to everyone who signs in this way">
          {(a) => (
            <RolesInput
              id={a.id}
              value={p.defaultRoles}
              onChange={p.onDefaultRoles}
              roles={p.roles}
              label="Add a default role"
              owner="default roles"
              disabled={p.locked}
              invalid={!!a['aria-invalid']}
              describedBy={a['aria-describedby']}
            />
          )}
        </Field>
        <Field id={`${p.id}-allowed`} label={label('Allowed groups', 'allowed_groups', p.ff)} error={p.errors.allowed_groups} hint="When set, only members of these groups may sign in">
          {(a) => (
            <ListInput id={a.id} describedBy={a['aria-describedby']} invalid={!!a['aria-invalid']} itemLabel="group" values={p.allowedGroups} onChange={p.onAllowedGroups} disabled={p.locked} placeholder="hangar-users" />
          )}
        </Field>
      </div>
      <label className="checkbox">
        <input type="checkbox" checked={p.autoCreate} onChange={(e) => p.onAutoCreate(e.target.checked)} disabled={p.locked} />
        {label('Create accounts on first sign-in', 'auto_create', p.ff)}
      </label>
    </section>
  );
}

/* ---------------------------------------------------------------- LDAP */

function LdapPanel({ canEdit }: { canEdit: boolean }) {
  const q = useLdapSettings();
  return (
    <Panel title="LDAP directory" titleId="ldap-title" aria-busy={q.isPending || undefined}>
      {q.isError ? (
        <ErrorNote what="the LDAP settings" onRetry={() => void q.refetch()} />
      ) : q.data ? (
        <LdapEditor key={q.data.source} data={q.data} canEdit={canEdit} />
      ) : (
        <p className="meta">Loading…</p>
      )}
    </Panel>
  );
}

function LdapEditor({ data, canEdit }: { data: LDAPSettings; canEdit: boolean }) {
  const id = useId();
  const roles = useRoleNames();
  const [form, setForm] = useState<LdapForm>(() => ldapForm(data));
  const [errors, setErrors] = useState<Errors>({});
  const [saved, setSaved] = useState<string | null>(null);
  const [testUser, setTestUser] = useState('');
  const save = useSaveLdap();
  const test = useTestLdap();
  const file = data.source === 'file';
  const locked = file || !canEdit;
  const ff = data.file_fields;
  const set = <K extends keyof LdapForm>(k: K, v: LdapForm[K]) => {
    setForm((f) => ({ ...f, [k]: v }));
    setSaved(null);
  };
  const err = (k: string) => errors[k];

  const submit = (e: FormEvent) => {
    e.preventDefault();
    const found = validateLdap(form, data.bind_password_configured);
    setErrors(found);
    setSaved(null);
    if (Object.keys(found).length) return;
    save.mutate(ldapInput(form), {
      onSuccess: (s) => {
        setForm((f) => ({ ...f, bind_password: '', clear_bind_password: false }));
        setSaved(s.enabled ? 'Saved. LDAP sign-in is on for every node.' : 'Saved. LDAP sign-in is off.');
      },
      onError: (e2) => setErrors(fieldErrorsOf(e2)),
    });
  };
  const runTest = () => {
    const found = validateLdap({ ...form, enabled: true }, data.bind_password_configured);
    delete found.bind_password;
    if (!file && Object.keys(found).length) {
      setErrors(found);
      return;
    }
    test.mutate({ settings: file ? undefined : ldapInput(form), username: testUser.trim() || undefined }, { onError: (e2) => setErrors(fieldErrorsOf(e2)) });
  };

  const text = (k: keyof LdapForm & string, title: string, hint?: string, mono = true) => (
    <Field id={`${id}-${k}`} label={label(title, k, ff)} error={err(k)} hint={hint}>
      {(a) => (
        <input
          {...a}
          className={mono ? 'input input--mono' : 'input'}
          autoComplete="off"
          spellCheck={false}
          value={form[k] as string}
          onChange={(e) => set(k, e.target.value as never)}
        />
      )}
    </Field>
  );

  return (
    <form className="settings-form" onSubmit={submit} noValidate aria-labelledby="ldap-title">
      <SourceNote source={data.source} what="LDAP sign-in" file="auth.ldap" updatedAt={data.updated_at} updatedBy={data.updated_by} />
      <fieldset className="settings-fieldset" disabled={locked}>
        <legend className="visually-hidden">LDAP settings</legend>
        <Toggle checked={form.enabled} onChange={(v) => set('enabled', v)} label={label('Allow sign-in with directory accounts', 'enabled', ff)} disabled={locked} />
        <div className="form-grid">
          {text('url', 'Directory URL', 'ldaps://dc1.example.com or ldap://dc1.example.com:389')}
          <div className="field settings-checks">
            <span className="field__label">Connection</span>
            <label className="checkbox">
              <input type="checkbox" checked={form.start_tls} onChange={(e) => set('start_tls', e.target.checked)} aria-describedby={err('start_tls') ? `${id}-stls` : undefined} />
              {label('Upgrade with StartTLS', 'start_tls', ff)}
            </label>
            {err('start_tls') && (
              <span id={`${id}-stls`} className="field__error">
                {err('start_tls')}
              </span>
            )}
          </div>
          {text('bind_dn', 'Bind DN', 'Service account that searches users; empty for anonymous search')}
          <Field
            id={`${id}-bindpw`}
            label={label('Bind password', 'bind_password', ff)}
            error={err('bind_password')}
            hint={data.bind_password_configured ? 'A password is stored. Type a new one to replace it.' : undefined}
          >
            {(a) => (
              <SecretInput
                id={a.id}
                a11y={a}
                configured={data.bind_password_configured}
                value={form.bind_password}
                onChange={(v) => set('bind_password', v)}
                cleared={form.clear_bind_password}
                onClear={(v) => set('clear_bind_password', v)}
                disabled={locked}
              />
            )}
          </Field>
          {text('user_base_dn', 'User base DN', 'e.g. OU=People,DC=example,DC=com')}
          {text('user_filter', 'User filter', '{username} is replaced by the login name')}
        </div>

        <h3 className="section-label">Attributes</h3>
        <div className="form-grid form-grid--3">
          {text('username_attribute', 'Username')}
          {text('email_attribute', 'Email')}
          {text('name_attribute', 'Display name')}
        </div>

        <h3 className="section-label">Groups</h3>
        <div className="form-grid">
          {text('group_attribute', 'Group attribute', 'Read from the user entry when no group base DN is set')}
          {text('group_base_dn', 'Group base DN', 'Search groups here instead')}
          {text('group_filter', 'Group filter', '{dn} is the user DN, {username} the login name')}
          {text('group_name_attribute', 'Group name attribute')}
        </div>
        <label className="checkbox">
          <input type="checkbox" checked={form.nested_groups} onChange={(e) => set('nested_groups', e.target.checked)} />
          {label('Resolve nested groups (Active Directory)', 'nested_groups', ff)}
        </label>

        <MappingSection
          id={id}
          ff={ff}
          rows={form.group_roles}
          defaultRoles={form.default_roles}
          allowedGroups={form.allowed_groups}
          autoCreate={form.auto_create}
          roles={roles}
          errors={errors}
          locked={locked}
          groupPlaceholder="Hangar Admins"
          onRows={(v) => set('group_roles', v)}
          onDefaultRoles={(v) => set('default_roles', v)}
          onAllowedGroups={(v) => set('allowed_groups', v)}
          onAutoCreate={(v) => set('auto_create', v)}
        />

        <details className="advanced" open={!!form.ca_pem || !!data.ca_file}>
          <summary className="advanced__summary">Trusted certificates</summary>
          {data.ca_file && (
            <p className="meta">
              The configuration file trusts <code>{data.ca_file}</code>.
            </p>
          )}
          <Field id={`${id}-ca`} label={label('CA certificates', 'ca_pem', ff)} error={err('ca_pem')} optional hint="PEM; for ldaps:// or StartTLS with a private certificate authority">
            {(a) => <textarea {...a} className="input input--mono textarea" rows={4} spellCheck={false} placeholder="-----BEGIN CERTIFICATE-----" value={form.ca_pem} onChange={(e) => set('ca_pem', e.target.value)} />}
          </Field>
        </details>
      </fieldset>

      {test.data && <TestResultView result={test.data} />}
      {test.isError && Object.keys(fieldErrorsOf(test.error)).length === 0 && (
        <p className="form-error" role="alert">
          {errorMessage(test.error)}
        </p>
      )}
      <SaveBar busy={save.isPending} canEdit={!locked} canTest={canEdit} onTest={runTest} testing={test.isPending} saved={saved} error={save.error}>
        <span className="settings-actions__probe">
          <label htmlFor={`${id}-testuser`} className="visually-hidden">
            Username to look up in the test
          </label>
          <input
            id={`${id}-testuser`}
            className="input"
            placeholder="Username to look up (optional)"
            autoComplete="off"
            value={testUser}
            onChange={(e) => setTestUser(e.target.value)}
          />
        </span>
      </SaveBar>
    </form>
  );
}
