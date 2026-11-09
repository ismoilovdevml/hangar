/**
 * Settings fields shared by "New repository" and repository settings, so both
 * screens edit a repository with exactly the same form.
 */
import { useState, type Dispatch, type ReactNode, type SetStateAction } from 'react';
import { useCleanupPolicies, useRepositoriesByFormat } from '../../api/hooks';
import type { WritePolicy } from '../../api/types';
import { IconArrowDown, IconArrowUp, IconClose } from '../../components/icons';
import { Pill } from '../../components/Pill';
import { Select } from '../../components/Select';
import { FORMAT_META } from '../../lib/format';
import {
  GROUP_WRITE_FORMATS,
  TYPE_META,
  WRITE_POLICY_LABEL,
  type AuthKind,
  type RepoForm,
  type RepoFormErrors,
  type RepoFormField,
} from '../../lib/repositories';
import { ProxyChoice } from '../settings/ProxyChoice';
import { FormatSettings } from './FormatFields';
import { errorId, fieldId, invalidProps } from './repoFormHelpers';
import './repoForm.css';

const AUTH_KINDS: { value: AuthKind; label: string }[] = [
  { value: 'none', label: 'None (public registry)' },
  { value: 'basic', label: 'Username and password' },
  { value: 'bearer', label: 'Bearer token' },
  { value: 'header', label: 'Token in a custom header' },
];

export function FieldError({ field, errors }: { field: RepoFormField; errors: RepoFormErrors }) {
  const msg = errors[field];
  return msg ? (
    <span id={errorId(field)} className="field__error">
      {msg}
    </span>
  ) : null;
}

export type Setter = <K extends keyof RepoForm>(key: K, value: RepoForm[K]) => void;
type Blur = (f: RepoFormField) => () => void;

interface RepoSettingsFieldsProps {
  form: RepoForm;
  /** Name of the repository being edited (excluded from group candidates); the typed name when creating. */
  selfName: string;
  set: Setter;
  setForm: Dispatch<SetStateAction<RepoForm>>;
  errors: RepoFormErrors;
  blur: Blur;
  /** Name and blob store: editable when creating, read-only in settings. */
  identity: ReactNode;
  /** Settings: the server already holds the upstream secret, so leaving it empty keeps it. */
  secretKept?: boolean;
  disabled?: boolean;
}

export function RepoSettingsFields({ form, selfName, set, setForm, errors, blur, identity, secretKept, disabled }: RepoSettingsFieldsProps) {
  const policies = useCleanupPolicies();
  const policyOptions = (policies.data?.items ?? []).filter((p) => !p.format || p.format === form.format);
  // A stored policy that no longer exists (or is not listed) stays selectable, so saving does not drop it.
  const missingPolicy = form.cleanupPolicy && policies.data && !policyOptions.some((p) => p.name === form.cleanupPolicy);

  return (
    <fieldset className="settings-fieldset" disabled={disabled}>
      <legend className="visually-hidden">Repository settings</legend>
      <div className="form-grid">
        {identity}

        <div className="field field--wide">
          <label htmlFor="repo-description" className="field__label">
            Description <span className="field__optional">optional</span>
          </label>
          <input id="repo-description" className="input" type="text" value={form.description} onChange={(e) => set('description', e.target.value)} />
        </div>

        {form.type === 'hosted' && (
          <div className="field">
            <label htmlFor="repo-writePolicy" className="field__label">Re-uploads</label>
            <Select<WritePolicy>
              id="repo-writePolicy"
              value={form.writePolicy}
              options={(Object.keys(WRITE_POLICY_LABEL) as WritePolicy[]).map((p) => ({ value: p, label: WRITE_POLICY_LABEL[p] }))}
              onChange={(v) => set('writePolicy', v)}
            />
          </div>
        )}

        {form.type === 'proxy' && <ProxySettings form={form} set={set} errors={errors} blur={blur} secretKept={secretKept} />}

        {form.type === 'group' && <GroupMembers form={form} selfName={selfName} setForm={setForm} set={set} errors={errors} />}

        <FormatSettings form={form} set={set} errors={errors} blur={blur} />

        <div className="field">
          <label htmlFor="repo-cleanup" className="field__label">Cleanup policy</label>
          <Select
            id="repo-cleanup"
            value={form.cleanupPolicy}
            onChange={(v) => set('cleanupPolicy', v)}
            disabled={!policies.data && !form.cleanupPolicy}
            aria-describedby="repo-cleanup-hint"
            options={[
              { value: '', label: 'Keep everything' },
              ...(missingPolicy || (!policies.data && form.cleanupPolicy) ? [{ value: form.cleanupPolicy, label: form.cleanupPolicy }] : []),
              ...policyOptions.map((p) => ({ value: p.name, label: p.name, hint: p.description || undefined })),
            ]}
          />
          <span id="repo-cleanup-hint" className="field__hint">
            {policies.isError
              ? 'Cleanup policies are not available on this server yet.'
              : policies.data && policyOptions.length === 0
                ? 'No cleanup policies for this format yet.'
                : 'What gets deleted, and when'}
          </span>
        </div>
      </div>

      <fieldset className="toggles">
        <legend className="visually-hidden">Security and access</legend>
        <label className="checkbox">
          <input type="checkbox" checked={form.scanOnArrival} onChange={(e) => set('scanOnArrival', e.target.checked)} />
          Scan new packages for vulnerabilities
        </label>
        <label className="checkbox">
          <input type="checkbox" checked={form.quarantineCritical} onChange={(e) => set('quarantineCritical', e.target.checked)} />
          Quarantine packages with critical issues
        </label>
        <label className="checkbox">
          <input type="checkbox" checked={form.anonymousRead} onChange={(e) => set('anonymousRead', e.target.checked)} />
          Allow anonymous read
        </label>
      </fieldset>
    </fieldset>
  );
}

function GroupMembers({
  form,
  selfName,
  setForm,
  set,
  errors,
}: {
  form: RepoForm;
  selfName: string;
  setForm: Dispatch<SetStateAction<RepoForm>>;
  set: Setter;
  errors: RepoFormErrors;
}) {
  const [candidate, setCandidate] = useState('');
  const repos = useRepositoriesByFormat(form.format);
  const memberTypes = new Map((repos.data ?? []).map((r) => [r.name, r.type] as const));
  const candidates = (repos.data ?? []).filter((r) => r.name !== selfName && !form.members.includes(r.name));
  const hostedMembers = form.members.filter((m) => memberTypes.get(m) === 'hosted');

  const moveMember = (i: number, d: -1 | 1) =>
    setForm((f) => {
      const members = [...f.members];
      const j = i + d;
      if (j < 0 || j >= members.length) return f;
      [members[i], members[j]] = [members[j]!, members[i]!];
      return { ...f, members };
    });

  return (
    <>
      <div className="field field--wide">
        <span className="field__label" id="members-label">Members, in the order reads are resolved</span>
        {form.members.length > 0 ? (
          <ol className="members" aria-labelledby="members-label">
            {form.members.map((m, i) => {
              const t = memberTypes.get(m);
              return (
                <li key={m} className="members__row">
                  <span className="members__pos" aria-hidden="true">{i + 1}</span>
                  <span className="members__name">{m}</span>
                  {t && <Pill tone={TYPE_META[t].tone}>{TYPE_META[t].label}</Pill>}
                  <span className="members__actions">
                    <button type="button" className="icon-button icon-button--sm" aria-label={`Move ${m} up`} disabled={i === 0} onClick={() => moveMember(i, -1)}>
                      <IconArrowUp size={16} />
                    </button>
                    <button type="button" className="icon-button icon-button--sm" aria-label={`Move ${m} down`} disabled={i === form.members.length - 1} onClick={() => moveMember(i, 1)}>
                      <IconArrowDown size={16} />
                    </button>
                    <button
                      type="button"
                      className="icon-button icon-button--sm"
                      aria-label={`Remove ${m}`}
                      onClick={() =>
                        setForm((f) => ({
                          ...f,
                          members: f.members.filter((x) => x !== m),
                          writeMember: f.writeMember === m ? '' : f.writeMember,
                        }))
                      }
                    >
                      <IconClose size={16} />
                    </button>
                  </span>
                </li>
              );
            })}
          </ol>
        ) : (
          <p className="meta">No members yet.</p>
        )}
        <div className="members__add">
          <label htmlFor={fieldId('members')} className="visually-hidden">Repository to add</label>
          <Select
            value={candidate}
            onChange={setCandidate}
            disabled={!form.format || repos.isPending || candidates.length === 0}
            placeholder={
              !form.format
                ? 'Pick a format first'
                : repos.isPending
                  ? 'Loading repositories…'
                  : candidates.length === 0
                    ? `No other ${FORMAT_META[form.format].label} repositories`
                    : 'Choose a repository…'
            }
            options={candidates.map((r) => ({ value: r.name, label: r.name, hint: TYPE_META[r.type].label }))}
            {...invalidProps('members', errors)}
          />
          <button
            type="button"
            className="button button--secondary"
            disabled={!candidate}
            onClick={() => {
              setForm((f) => ({ ...f, members: [...f.members, candidate] }));
              setCandidate('');
            }}
          >
            Add member
          </button>
        </div>
        <FieldError field="members" errors={errors} />
      </div>

      {form.format && GROUP_WRITE_FORMATS.includes(form.format) && (
        <div className="field">
          <label htmlFor={fieldId('writeMember')} className="field__label">Publishing through the group</label>
          <Select
            value={form.writeMember}
            onChange={(v) => set('writeMember', v)}
            options={[{ value: '', label: 'Read-only group' }, ...hostedMembers.map((m) => ({ value: m, label: `Send uploads to ${m}` }))]}
            {...invalidProps('writeMember', errors, 'repo-write-hint')}
          />
          <span id="repo-write-hint" className="field__hint">Only hosted members can receive uploads</span>
          <FieldError field="writeMember" errors={errors} />
        </div>
      )}
    </>
  );
}

function ProxySettings({
  form,
  set,
  errors,
  blur,
  secretKept,
}: {
  form: RepoForm;
  set: Setter;
  errors: RepoFormErrors;
  blur: Blur;
  secretKept?: boolean;
}) {
  const ttl = (field: 'metadataTtl' | 'contentTtl' | 'negativeTtl', label: string, hint: string) => (
    <div className="field">
      <label htmlFor={fieldId(field)} className="field__label">{label}</label>
      <input
        className="input input--mono"
        type="text"
        inputMode="text"
        autoComplete="off"
        spellCheck={false}
        value={form[field]}
        onChange={(e) => set(field, e.target.value)}
        onBlur={blur(field)}
        {...invalidProps(field, errors, `${fieldId(field)}-hint`)}
      />
      <span id={`${fieldId(field)}-hint`} className="field__hint">{hint}</span>
      <FieldError field={field} errors={errors} />
    </div>
  );
  const keptHint = secretKept ? 'Stored encrypted. Leave empty to keep it.' : undefined;

  return (
    <>
      <div className="field field--wide">
        <label htmlFor={fieldId('remoteUrl')} className="field__label">Remote URL</label>
        <input
          className="input input--mono"
          type="url"
          autoComplete="off"
          spellCheck={false}
          placeholder="https://"
          value={form.remoteUrl}
          onChange={(e) => set('remoteUrl', e.target.value)}
          onBlur={blur('remoteUrl')}
          {...invalidProps('remoteUrl', errors)}
        />
        <FieldError field="remoteUrl" errors={errors} />
      </div>
      <div className="form-grid form-grid--3 field--wide">
        {ttl('metadataTtl', 'Metadata cached for', 'Indexes and tag lists')}
        {ttl('contentTtl', 'Content cached for', '0s keeps files forever')}
        {ttl('negativeTtl', 'Not-found cached for', 'Remembered 404s')}
      </div>

      <details className="advanced field--wide" open={form.authKind !== 'none' || form.proxyMode !== 'inherit' || !!form.caBundle}>
        <summary className="advanced__summary">Upstream credentials and connection</summary>
        <div className="form-grid">
          <div className="field">
            <label htmlFor="repo-authKind" className="field__label">Authentication</label>
            <Select<AuthKind> id="repo-authKind" value={form.authKind} options={AUTH_KINDS} onChange={(v) => set('authKind', v)} />
            <span className="field__hint">Sent only to the remote's own host. Stored encrypted, never shown again.</span>
          </div>
          {form.authKind === 'basic' && (
            <>
              <div className="field">
                <label htmlFor={fieldId('authUsername')} className="field__label">Username</label>
                <input className="input" type="text" autoComplete="off" value={form.authUsername} onChange={(e) => set('authUsername', e.target.value)} onBlur={blur('authUsername')} {...invalidProps('authUsername', errors)} />
                <FieldError field="authUsername" errors={errors} />
              </div>
              <div className="field">
                <label htmlFor={fieldId('authPassword')} className="field__label">Password</label>
                <input className="input" type="password" autoComplete="new-password" placeholder={secretKept ? '••••••••' : undefined} value={form.authPassword} onChange={(e) => set('authPassword', e.target.value)} onBlur={blur('authPassword')} {...invalidProps('authPassword', errors, keptHint ? 'repo-authPassword-hint' : undefined)} />
                {keptHint && <span id="repo-authPassword-hint" className="field__hint">{keptHint}</span>}
                <FieldError field="authPassword" errors={errors} />
              </div>
            </>
          )}
          {form.authKind === 'header' && (
            <div className="field">
              <label htmlFor={fieldId('authHeader')} className="field__label">Header name</label>
              <input className="input input--mono" type="text" autoComplete="off" spellCheck={false} placeholder="Private-Token" value={form.authHeader} onChange={(e) => set('authHeader', e.target.value)} onBlur={blur('authHeader')} {...invalidProps('authHeader', errors)} />
              <FieldError field="authHeader" errors={errors} />
            </div>
          )}
          {(form.authKind === 'bearer' || form.authKind === 'header') && (
            <div className="field">
              <label htmlFor={fieldId('authToken')} className="field__label">{form.authKind === 'header' ? 'Header value' : 'Token'}</label>
              <input className="input" type="password" autoComplete="new-password" placeholder={secretKept ? '••••••••' : undefined} value={form.authToken} onChange={(e) => set('authToken', e.target.value)} onBlur={blur('authToken')} {...invalidProps('authToken', errors, keptHint ? 'repo-authToken-hint' : undefined)} />
              {keptHint && <span id="repo-authToken-hint" className="field__hint">{keptHint}</span>}
              <FieldError field="authToken" errors={errors} />
            </div>
          )}
        </div>

        <div className="field">
          <label htmlFor={fieldId('proxyName')} className="field__label">Outbound proxy</label>
          <ProxyChoice
            id={fieldId('proxyName')}
            mode={form.proxyMode}
            proxyName={form.proxyName}
            onChange={(mode, name) => {
              set('proxyMode', mode);
              set('proxyName', name);
            }}
            invalid={!!errors.proxyName}
            describedBy={errors.proxyName ? errorId('proxyName') : 'repo-proxy-hint'}
          />
          {!errors.proxyName && (
            <span id="repo-proxy-hint" className="field__hint">
              Proxies are managed under Settings → Proxies
            </span>
          )}
          <FieldError field="proxyName" errors={errors} />
        </div>
        {form.proxyMode === 'url' && (
          <div className="field">
            <label htmlFor={fieldId('proxyUrl')} className="field__label">Proxy URL</label>
            <input className="input input--mono" type="url" autoComplete="off" spellCheck={false} placeholder="http://proxy.internal:3128" value={form.proxyUrl} onChange={(e) => set('proxyUrl', e.target.value)} onBlur={blur('proxyUrl')} {...invalidProps('proxyUrl', errors, 'repo-proxyUrl-hint')} />
            <span id="repo-proxyUrl-hint" className="field__hint">http://, https:// or socks5://; a password in the URL is stored encrypted</span>
            <FieldError field="proxyUrl" errors={errors} />
          </div>
        )}
        <div className="field">
          <label htmlFor={fieldId('caBundle')} className="field__label">
            Extra trusted certificates <span className="field__optional">optional</span>
          </label>
          <textarea
            className="input input--mono textarea"
            rows={4}
            spellCheck={false}
            placeholder="-----BEGIN CERTIFICATE-----"
            value={form.caBundle}
            onChange={(e) => set('caBundle', e.target.value)}
            onBlur={blur('caBundle')}
            {...invalidProps('caBundle', errors, 'repo-ca-hint')}
          />
          <span id="repo-ca-hint" className="field__hint">PEM, trusted for this remote in addition to the system roots (private CA, TLS-inspecting proxy)</span>
          <FieldError field="caBundle" errors={errors} />
        </div>
      </details>
    </>
  );
}
