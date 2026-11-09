/**
 * The editor card of Settings → Authentication: the few fields a preset
 * leaves to the administrator, a summary of what the preset fills in, and
 * everything else one "Customize" away.
 */
import { useId, useState, type FormEvent, type ReactNode } from 'react';
import { CopyButton } from '../../components/CopyButton';
import { Panel } from '../../components/Panel';
import { Select } from '../../components/Select';
import { formatUtc } from '../../lib/tasks';
import { useCanSystem } from '../../api/ops';
import { useRoles } from '../access/api';
import { Field, FileBadge, ListInput, RoleMappingTable, RolesInput, SecretInput, TestResultView, Toggle } from './controls';
import { ldapForm, ldapInput, oidcForm, oidcInput, validateLdap, validateOidc, type Errors, type LdapForm, type OidcForm } from './model';
import { inferLdapPreset, inferOidcPreset, ldapPreset, ldapSummary, oidcPreset, oidcSummary, withBindDn, type LdapPreset, type OidcPreset } from './presets';
import {
  errorMessage,
  fieldErrorsOf,
  useSaveLdap,
  useSaveOidc,
  useTestLdap,
  useTestOidc,
  type GroupRole,
  type LDAPSettings,
  type OIDCSettings,
} from './settingsApi';

/* ---------------------------------------------------------------- shared */

/** Names of the existing roles, for the role pickers (empty when roles cannot be listed). */
function useRoleNames(): string[] {
  const roles = useRoles(useCanSystem('users', 'read'));
  return (roles.data?.items ?? []).map((r) => r.name);
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

function FileNote({ what, section, env }: { what: string; section: string; env: string }) {
  return (
    <p className="note settings-file-note">
      {what} is set in the configuration file (<code>{section}</code> in hangar.yaml or <code>{env}</code>) and is shown read-only. Change it there and restart
      Hangar.
    </p>
  );
}

const FIELDS_OIDC_BASIC = new Set(['issuer_url', 'client_id', 'client_secret']);
const FIELDS_LDAP_BASIC = new Set(['url', 'bind_dn', 'bind_password']);
const isMapping = (k: string) => k.startsWith('group_roles') || k === 'default_roles' || k === 'allowed_groups' || k === 'auto_create';

/** Which hidden sections an error set needs opened. */
function sectionsFor(errors: Errors, basic: Set<string>) {
  const keys = Object.keys(errors);
  return { more: keys.some((k) => !basic.has(k) && !isMapping(k) && k !== 'enabled'), roles: keys.some(isMapping) };
}

interface Chrome {
  /** Heading id; the form is labelled by it. */
  titleId: string;
  /** "Other" shows its protocol switch here. */
  protocolSwitch?: ReactNode;
}

function EditorHead({ titleId, name, sub, toggle }: { titleId: string; name: string; sub: ReactNode; toggle?: ReactNode }) {
  return (
    <div className="auth-editor__head">
      <div className="auth-editor__heading">
        <h2 id={titleId} className="auth-editor__title">
          {name}
        </h2>
        <p className="auth-editor__sub">{sub}</p>
      </div>
      {toggle}
    </div>
  );
}

function Summary({ items, open, onToggle, controls }: { items: string[]; open: boolean; onToggle: () => void; controls: string }) {
  return (
    <div className="auth-summary">
      <span>We’ll use:</span>
      <ul className="auth-summary__list" aria-label="Preset values">
        {items.filter(Boolean).map((v) => (
          <li key={v} className="auth-summary__chip">
            {v}
          </li>
        ))}
      </ul>
      <button type="button" className="link-button auth-summary__toggle" aria-expanded={open} aria-controls={controls} onClick={onToggle}>
        {open ? 'Hide details' : 'Customize'}
      </button>
    </div>
  );
}

/** The post-logout redirect URI that matches a callback URI: <base>/login. */
function postLogoutUri(redirectUri?: string): string {
  const base = redirectUri?.replace(/\/api\/v1\/auth\/oidc\/callback$/, '');
  return base && base !== redirectUri ? `${base}/login` : '';
}

function SetupSteps({ preset, redirectUri, open }: { preset: { name: string; id: string; steps: string[] }; redirectUri?: string; open: boolean }) {
  const where = preset.id === 'other' ? 'your provider' : preset.name;
  return (
    <div className="auth-setup">
      {redirectUri && (
        <div className="redirect-uri">
          <span className="field__label">Redirect URI to register at {where}</span>
          <span className="redirect-uri__value">
            <code>{redirectUri}</code>
            <CopyButton text={redirectUri} label="Copy the redirect URI" />
          </span>
        </div>
      )}
      <details className="auth-setup__steps" open={open}>
        <summary className="auth-setup__summary">How to set up {where}</summary>
        <ol>
          {preset.steps.map((s) => (
            <li key={s}>{s}</li>
          ))}
        </ol>
      </details>
    </div>
  );
}

function MappingSection(p: {
  id: string;
  ff: readonly string[];
  rows: GroupRole[];
  defaultRoles: string[];
  allowedGroups: string[];
  autoCreate: boolean;
  errors: Errors;
  locked: boolean;
  groupPlaceholder: string;
  onRows: (v: GroupRole[]) => void;
  onDefaultRoles: (v: string[]) => void;
  onAllowedGroups: (v: string[]) => void;
  onAutoCreate: (v: boolean) => void;
}) {
  const roles = useRoleNames();
  return (
    <section id={`${p.id}-roles`} className="settings-subsection auth-editor__section" aria-label="Groups and roles">
      <h3 className="section-label">{label('Group → role mapping', 'group_roles', p.ff)}</h3>
      <p className="meta">Members of a group get its roles at every sign-in. Group names match case-insensitively, with or without a leading "/".</p>
      <RoleMappingTable rows={p.rows} onChange={p.onRows} roles={roles} errors={p.errors} disabled={p.locked} groupPlaceholder={p.groupPlaceholder} />
      {p.errors.group_roles && <span className="field__error">{p.errors.group_roles}</span>}
      <div className="form-grid">
        <Field id={`${p.id}-defroles`} label={label('Default roles', 'default_roles', p.ff)} error={p.errors.default_roles} hint="Granted to everyone who signs in this way">
          {(a) => (
            <RolesInput
              id={a.id}
              value={p.defaultRoles}
              onChange={p.onDefaultRoles}
              roles={roles}
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

function CaField({ id, ff, value, onChange, error, caFile, hint }: { id: string; ff: readonly string[]; value: string; onChange: (v: string) => void; error?: string; caFile?: string; hint: string }) {
  return (
    <>
      {caFile && (
        <p className="meta">
          The configuration file trusts <code>{caFile}</code>.
        </p>
      )}
      <Field id={`${id}-ca`} label={label('CA certificates', 'ca_pem', ff)} error={error} optional hint={hint}>
        {(a) => <textarea {...a} className="input input--mono textarea" rows={4} spellCheck={false} placeholder="-----BEGIN CERTIFICATE-----" value={value} onChange={(e) => onChange(e.target.value)} />}
      </Field>
    </>
  );
}

/** "New users get Reader · Groups and roles" plus the buttons. */
function Footer(p: {
  id: string;
  form: { default_roles: string[]; group_roles: GroupRole[]; auto_create: boolean };
  rolesOpen: boolean;
  onRoles: () => void;
  canEdit: boolean;
  canTest: boolean;
  canSave: boolean;
  onTest: () => void;
  testing: boolean;
  saving: boolean;
  saveLabel: string;
  probe?: ReactNode;
}) {
  const def = p.form.default_roles;
  const mapped = p.form.group_roles.length;
  return (
    <div className="auth-editor__footer">
      <p className="auth-editor__who">
        {!p.form.auto_create ? (
          'Only people who signed in before are admitted'
        ) : def.length ? (
          <>
            New users get <b>{def.join(', ')}</b>
          </>
        ) : (
          'New users get no role until a group matches'
        )}
        {' · '}
        <button type="button" className="link-button" aria-expanded={p.rolesOpen} aria-controls={`${p.id}-roles`} onClick={p.onRoles}>
          Groups and roles{mapped > 0 && ` (${mapped} mapped)`}
        </button>
      </p>
      {!p.canEdit && <p className="meta auth-editor__readonly">Read-only: changing sign-in needs system:settings:admin.</p>}
      {p.canTest && (
        <>
          {p.probe}
          <button type="button" className="button button--secondary auth-editor__button" onClick={p.onTest} disabled={p.testing}>
            {p.testing ? 'Testing…' : 'Test'}
          </button>
        </>
      )}
      {p.canSave && (
        <button type="submit" className="button button--primary auth-editor__button" disabled={p.saving}>
          {p.saving ? 'Saving…' : p.saveLabel}
        </button>
      )}
    </div>
  );
}

function Messages({ saved, error, test }: { saved: string | null; error: unknown; test: ReturnType<typeof useTestOidc> | ReturnType<typeof useTestLdap> }) {
  return (
    <>
      {test.data && <TestResultView result={test.data} />}
      {test.isError && Object.keys(fieldErrorsOf(test.error)).length === 0 && (
        <p className="form-error" role="alert">
          {errorMessage(test.error)}
        </p>
      )}
      {saved && (
        <p className="form-success" role="status">
          {saved}
        </p>
      )}
      {!!error && (
        <p className="form-error" role="alert">
          {errorMessage(error)}
        </p>
      )}
    </>
  );
}

/** Where a stored provider stands, as the subtitle says it. */
function storedLine(enabled: boolean, updatedAt?: string | null, updatedBy?: string): ReactNode {
  return (
    <>
      {enabled ? 'In use.' : 'Saved, sign-in is off.'}
      {updatedAt && ` Last saved ${formatUtc(updatedAt)}${updatedBy ? ` by ${updatedBy}` : ''}.`}
    </>
  );
}

/* ---------------------------------------------------------------- OIDC */

export function OidcEditor({
  preset,
  data,
  form,
  setForm,
  canEdit,
  titleId,
  protocolSwitch,
}: Chrome & {
  preset: OidcPreset;
  data: OIDCSettings;
  form: OidcForm;
  setForm: (fn: (f: OidcForm) => OidcForm) => void;
  canEdit: boolean;
}) {
  const id = useId();
  const [errors, setErrors] = useState<Errors>({});
  const [saved, setSaved] = useState<string | null>(null);
  const [more, setMore] = useState(preset.id === 'other');
  const [roles, setRoles] = useState(false);
  const save = useSaveOidc();
  const test = useTestOidc();

  const file = data.source === 'file';
  const stored = data.source !== 'default';
  const storedId = stored ? inferOidcPreset(data) : null;
  const same = storedId === preset.id;
  const storedName = storedId ? oidcPreset(storedId).name : '';
  const locked = file || !canEdit;
  const ff = data.file_fields;
  const adding = !same;
  const replacing = !file && stored && !same && data.enabled;
  const secretBelongsElsewhere = !same && data.client_secret_configured && !form.clear_client_secret;

  const set = <K extends keyof OidcForm>(k: K, v: OidcForm[K]) => {
    setForm((f) => ({ ...f, [k]: v }));
    setSaved(null);
  };
  const err = (k: string) => errors[k];
  const show = (found: Errors) => {
    setErrors(found);
    const s = sectionsFor(found, FIELDS_OIDC_BASIC);
    if (s.more) setMore(true);
    if (s.roles) setRoles(true);
  };
  const check = (f: OidcForm) => {
    const found = validateOidc(f);
    if (secretBelongsElsewhere && !f.client_secret) found.client_secret = `Enter the client secret ${preset.id === 'other' ? 'your provider' : preset.name} issued, or remove the stored one.`;
    return found;
  };

  const submit = (e: FormEvent) => {
    e.preventDefault();
    const next = { ...form, enabled: adding ? true : form.enabled };
    const found = check(next);
    show(found);
    setSaved(null);
    if (Object.keys(found).length) return;
    save.mutate(
      { ...oidcInput(next), preset: preset.id },
      {
        onSuccess: (s) => {
          // The stored secret is now the one to keep.
          setForm(() => oidcForm(s));
          setSaved(s.enabled ? `Saved. People can sign in with ${s.display_name} on every node.` : 'Saved. Single sign-on is off.');
        },
        onError: (e2) => show(fieldErrorsOf(e2)),
      },
    );
  };
  const runTest = () => {
    if (!file) {
      const found = check({ ...form, enabled: true });
      if (Object.keys(found).length) {
        show(found);
        return;
      }
    }
    test.mutate(file ? undefined : { ...oidcInput(form), preset: preset.id }, { onError: (e2) => show(fieldErrorsOf(e2)) });
  };

  const sub = file ? (
    same ? (
      'In use. Set in the configuration file.'
    ) : (
      `Single sign-on uses ${storedName} from the configuration file.`
    )
  ) : same ? (
    storedLine(data.enabled, data.updated_at, data.updated_by)
  ) : preset.id === 'other' ? (
    'Any OpenID Connect provider. Every field is yours to set.'
  ) : (
    `Only 3 things to fill in. The rest is set for ${preset.name}.`
  );

  // A file-managed provider is shown as it is; another preset cannot replace it here.
  if (file && !same) {
    return (
      <Panel as="div" className="auth-editor" role="region" aria-labelledby={titleId}>
        <EditorHead titleId={titleId} name={preset.name} sub={sub} />
        {protocolSwitch}
        <p className="note settings-file-note">
          Hangar signs in with one OpenID Connect provider, and the configuration file sets it (<code>auth.oidc</code> in hangar.yaml or{' '}
          <code>HANGAR_AUTH_OIDC_*</code>). To use {preset.id === 'other' ? 'another provider' : preset.name} instead, change it there and restart Hangar.
        </p>
        <SetupSteps preset={preset} redirectUri={data.redirect_uri} open />
      </Panel>
    );
  }

  return (
    <form className="panel glass auth-editor" onSubmit={submit} noValidate aria-labelledby={titleId}>
      <EditorHead
        titleId={titleId}
        name={preset.name}
        sub={sub}
        toggle={
          same && !file ? (
            <Toggle checked={form.enabled} onChange={(v) => set('enabled', v)} label={`Allow sign-in with ${preset.id === 'other' ? 'single sign-on' : preset.name}`} disabled={locked} />
          ) : undefined
        }
      />
      {protocolSwitch}
      {file && <FileNote what="Single sign-on" section="auth.oidc" env="HANGAR_AUTH_OIDC_*" />}
      {replacing && canEdit && (
        <p className="note">
          Hangar signs in with one OpenID Connect provider. Adding {preset.id === 'other' ? 'this provider' : preset.name} replaces {storedName} ({data.display_name}).
        </p>
      )}
      <fieldset className="settings-fieldset" disabled={locked}>
        <legend className="visually-hidden">{preset.name} settings</legend>
        <div className="form-grid auth-editor__basics">
          <Field id={`${id}-issuer`} wide label={label('Issuer URL', 'issuer_url', ff)} error={err('issuer_url')}>
            {(a) => (
              <input
                {...a}
                className="input input--mono"
                type="url"
                autoComplete="off"
                spellCheck={false}
                placeholder={preset.issuerPlaceholder}
                value={form.issuer_url}
                onChange={(e) => set('issuer_url', e.target.value)}
              />
            )}
          </Field>
          <Field id={`${id}-client`} label={label('Client ID', 'client_id', ff)} error={err('client_id')}>
            {(a) => <input {...a} className="input input--mono" autoComplete="off" spellCheck={false} placeholder="hangar" value={form.client_id} onChange={(e) => set('client_id', e.target.value)} />}
          </Field>
          <Field
            id={`${id}-secret`}
            label={label('Client secret', 'client_secret', ff)}
            error={err('client_secret')}
            hint={
              secretBelongsElsewhere
                ? `The stored secret belongs to ${storedName}.`
                : data.client_secret_configured
                  ? 'A secret is stored. Type a new one to replace it.'
                  : 'Leave empty for a public client (PKCE only).'
            }
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
        </div>

        <Summary items={oidcSummary(form)} open={more} onToggle={() => setMore((v) => !v)} controls={`${id}-more`} />

        {more && (
          <div id={`${id}-more`} className="auth-editor__section">
            <div className="form-grid">
              <Field id={`${id}-name`} label={label('Button label', 'display_name', ff)} error={err('display_name')} hint='Shown as "Continue with …" on the sign-in page'>
                {(a) => <input {...a} className="input" maxLength={64} value={form.display_name} onChange={(e) => set('display_name', e.target.value)} />}
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
              <Field
                id={`${id}-logout`}
                label={label('Sign out', 'logout', ff)}
                error={err('logout')}
                hint={postLogoutUri(data.redirect_uri) ? `Register ${postLogoutUri(data.redirect_uri)} as a post-logout redirect URI` : undefined}
              >
                {(a) => (
                  <Select
                    id={a.id}
                    aria-describedby={a['aria-describedby']}
                    value={form.logout}
                    onChange={(v) => set('logout', v)}
                    disabled={locked}
                    options={[
                      { value: 'provider', label: 'Also sign out of the provider', hint: 'When it has an end_session_endpoint' },
                      { value: 'local', label: 'Sign out of Hangar only' },
                    ]}
                  />
                )}
              </Field>
              <Field id={`${id}-scopes`} wide label={label('Scopes', 'scopes', ff)} error={err('scopes')} hint="openid is required">
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
            <CaField id={id} ff={ff} value={form.ca_pem} onChange={(v) => set('ca_pem', v)} error={err('ca_pem')} caFile={data.ca_file} hint="PEM; for a provider with a private certificate authority" />
          </div>
        )}

        <SetupSteps preset={preset} redirectUri={data.redirect_uri} open={!same && !locked} />

        {roles && (
          <MappingSection
            id={id}
            ff={ff}
            rows={form.group_roles}
            defaultRoles={form.default_roles}
            allowedGroups={form.allowed_groups}
            autoCreate={form.auto_create}
            errors={errors}
            locked={locked}
            groupPlaceholder={preset.groupPlaceholder}
            onRows={(v) => set('group_roles', v)}
            onDefaultRoles={(v) => set('default_roles', v)}
            onAllowedGroups={(v) => set('allowed_groups', v)}
            onAutoCreate={(v) => set('auto_create', v)}
          />
        )}
      </fieldset>

      <Messages saved={saved} error={save.error} test={test} />
      <Footer
        id={id}
        form={form}
        rolesOpen={roles}
        onRoles={() => setRoles((v) => !v)}
        canEdit={canEdit}
        canTest={canEdit}
        canSave={!locked}
        onTest={runTest}
        testing={test.isPending}
        saving={save.isPending}
        saveLabel={same ? 'Save' : replacing ? `Switch to ${preset.name}` : `Add ${preset.name}`}
      />
    </form>
  );
}

/* ---------------------------------------------------------------- LDAP */

export function LdapEditor({
  preset,
  data,
  form,
  setForm,
  canEdit,
  titleId,
  protocolSwitch,
}: Chrome & {
  preset: LdapPreset;
  data: LDAPSettings;
  form: LdapForm;
  setForm: (fn: (f: LdapForm) => LdapForm) => void;
  canEdit: boolean;
}) {
  const id = useId();
  const [errors, setErrors] = useState<Errors>({});
  const [saved, setSaved] = useState<string | null>(null);
  const [more, setMore] = useState(preset.id === 'other');
  const [roles, setRoles] = useState(false);
  const [testUser, setTestUser] = useState('');
  const save = useSaveLdap();
  const test = useTestLdap();

  const file = data.source === 'file';
  const stored = data.source !== 'default';
  const storedId = stored ? inferLdapPreset(data) : null;
  const same = storedId === preset.id;
  const storedName = storedId ? ldapPreset(storedId).name : '';
  const locked = file || !canEdit;
  const ff = data.file_fields;
  const adding = !same;
  const replacing = !file && stored && !same && data.enabled;
  const passwordBelongsElsewhere = !same && data.bind_password_configured && !form.clear_bind_password;

  const update = (fn: (f: LdapForm) => LdapForm) => {
    setForm(fn);
    setSaved(null);
  };
  const set = <K extends keyof LdapForm>(k: K, v: LdapForm[K]) => update((f) => ({ ...f, [k]: v }));
  const err = (k: string) => errors[k];
  const show = (found: Errors) => {
    setErrors(found);
    const s = sectionsFor(found, FIELDS_LDAP_BASIC);
    if (s.more) setMore(true);
    if (s.roles) setRoles(true);
  };
  const check = (f: LdapForm) => {
    const found = validateLdap(f, data.bind_password_configured && !passwordBelongsElsewhere);
    if (found.bind_password && passwordBelongsElsewhere) found.bind_password = `The stored password belongs to ${storedName}. Enter the service account’s password.`;
    return found;
  };

  const submit = (e: FormEvent) => {
    e.preventDefault();
    const next = { ...form, enabled: adding ? true : form.enabled };
    const found = check(next);
    show(found);
    setSaved(null);
    if (Object.keys(found).length) return;
    save.mutate(
      { ...ldapInput(next), preset: preset.id },
      {
        onSuccess: (s) => {
          setForm(() => ldapForm(s));
          setSaved(s.enabled ? `Saved. ${preset.id === 'other' ? 'Directory' : preset.name} sign-in is on for every node.` : 'Saved. LDAP sign-in is off.');
        },
        onError: (e2) => show(fieldErrorsOf(e2)),
      },
    );
  };
  const runTest = () => {
    if (!file) {
      const found = check({ ...form, enabled: true });
      delete found.bind_password;
      if (Object.keys(found).length) {
        show(found);
        return;
      }
    }
    test.mutate({ settings: file ? undefined : { ...ldapInput(form), preset: preset.id }, username: testUser.trim() || undefined }, { onError: (e2) => show(fieldErrorsOf(e2)) });
  };

  const text = (k: keyof LdapForm & string, title: string, hint?: string) => (
    <Field id={`${id}-${k}`} label={label(title, k, ff)} error={err(k)} hint={hint}>
      {(a) => <input {...a} className="input input--mono" autoComplete="off" spellCheck={false} value={form[k] as string} onChange={(e) => set(k, e.target.value as never)} />}
    </Field>
  );

  const sub = file ? (
    same ? (
      'In use. Set in the configuration file.'
    ) : (
      `Directory sign-in uses ${storedName} from the configuration file.`
    )
  ) : same ? (
    storedLine(data.enabled, data.updated_at, data.updated_by)
  ) : preset.id === 'other' ? (
    'Any LDAP directory. Every field is yours to set.'
  ) : (
    `Only 3 things to fill in. The rest is set for ${preset.name}.`
  );

  if (file && !same) {
    return (
      <Panel as="div" className="auth-editor" role="region" aria-labelledby={titleId}>
        <EditorHead titleId={titleId} name={preset.name} sub={sub} />
        {protocolSwitch}
        <p className="note settings-file-note">
          Hangar signs in with one directory, and the configuration file sets it (<code>auth.ldap</code> in hangar.yaml or <code>HANGAR_AUTH_LDAP_*</code>). To use{' '}
          {preset.id === 'other' ? 'another directory' : preset.name} instead, change it there and restart Hangar.
        </p>
        <SetupSteps preset={preset} open />
      </Panel>
    );
  }

  return (
    <form className="panel glass auth-editor" onSubmit={submit} noValidate aria-labelledby={titleId}>
      <EditorHead
        titleId={titleId}
        name={preset.name}
        sub={sub}
        toggle={
          same && !file ? (
            <Toggle checked={form.enabled} onChange={(v) => set('enabled', v)} label={`Allow sign-in with ${preset.id === 'other' ? 'directory accounts' : preset.name}`} disabled={locked} />
          ) : undefined
        }
      />
      {protocolSwitch}
      {file && <FileNote what="Directory sign-in" section="auth.ldap" env="HANGAR_AUTH_LDAP_*" />}
      {replacing && canEdit && (
        <p className="note">
          Hangar signs in with one directory. Adding {preset.id === 'other' ? 'this directory' : preset.name} replaces {storedName}.
        </p>
      )}
      <fieldset className="settings-fieldset" disabled={locked}>
        <legend className="visually-hidden">{preset.name} settings</legend>
        <div className="form-grid auth-editor__basics">
          <Field id={`${id}-url`} wide label={label('Server', 'url', ff)} error={err('url')}>
            {(a) => <input {...a} className="input input--mono" autoComplete="off" spellCheck={false} placeholder={preset.urlPlaceholder} value={form.url} onChange={(e) => set('url', e.target.value)} />}
          </Field>
          <Field id={`${id}-bind`} label={label('Service account', 'bind_dn', ff)} error={err('bind_dn')}>
            {(a) => (
              <input
                {...a}
                className="input input--mono"
                autoComplete="off"
                spellCheck={false}
                placeholder={preset.bindPlaceholder}
                value={form.bind_dn}
                onChange={(e) => update((f) => withBindDn(f, preset, e.target.value))}
              />
            )}
          </Field>
          <Field
            id={`${id}-bindpw`}
            label={label('Password', 'bind_password', ff)}
            error={err('bind_password')}
            hint={passwordBelongsElsewhere ? `The stored password belongs to ${storedName}.` : data.bind_password_configured ? 'A password is stored. Type a new one to replace it.' : undefined}
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
        </div>

        <Summary items={ldapSummary(form)} open={more} onToggle={() => setMore((v) => !v)} controls={`${id}-more`} />

        {more && (
          <div id={`${id}-more`} className="auth-editor__section">
            <div className="form-grid">
              {text('user_base_dn', 'User base DN', 'Where users are searched, e.g. OU=People,DC=example,DC=com')}
              {text('user_filter', 'User filter', '{username} is replaced by the login name')}
              <div className="field settings-checks">
                <span className="field__label">Connection</span>
                <label className="checkbox">
                  <input type="checkbox" checked={form.start_tls} onChange={(e) => set('start_tls', e.target.checked)} aria-describedby={err('start_tls') ? `${id}-stls` : undefined} />
                  {label('Upgrade ldap:// with StartTLS', 'start_tls', ff)}
                </label>
                {err('start_tls') && (
                  <span id={`${id}-stls`} className="field__error">
                    {err('start_tls')}
                  </span>
                )}
              </div>
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
            <CaField id={id} ff={ff} value={form.ca_pem} onChange={(v) => set('ca_pem', v)} error={err('ca_pem')} caFile={data.ca_file} hint="PEM; for ldaps:// or StartTLS with a private certificate authority" />
          </div>
        )}

        <SetupSteps preset={preset} open={!same && !locked} />

        {roles && (
          <MappingSection
            id={id}
            ff={ff}
            rows={form.group_roles}
            defaultRoles={form.default_roles}
            allowedGroups={form.allowed_groups}
            autoCreate={form.auto_create}
            errors={errors}
            locked={locked}
            groupPlaceholder={preset.groupPlaceholder}
            onRows={(v) => set('group_roles', v)}
            onDefaultRoles={(v) => set('default_roles', v)}
            onAllowedGroups={(v) => set('allowed_groups', v)}
            onAutoCreate={(v) => set('auto_create', v)}
          />
        )}
      </fieldset>

      <Messages saved={saved} error={save.error} test={test} />
      <Footer
        id={id}
        form={form}
        rolesOpen={roles}
        onRoles={() => setRoles((v) => !v)}
        canEdit={canEdit}
        canTest={canEdit}
        canSave={!locked}
        onTest={runTest}
        testing={test.isPending}
        saving={save.isPending}
        saveLabel={same ? 'Save' : replacing ? `Switch to ${preset.name}` : `Add ${preset.name}`}
        probe={
          <span className="auth-editor__probe">
            <label htmlFor={`${id}-testuser`} className="visually-hidden">
              Username to look up in the test
            </label>
            <input id={`${id}-testuser`} className="input" placeholder="Username to look up (optional)" autoComplete="off" value={testUser} onChange={(e) => setTestUser(e.target.value)} />
          </span>
        }
      />
    </form>
  );
}
