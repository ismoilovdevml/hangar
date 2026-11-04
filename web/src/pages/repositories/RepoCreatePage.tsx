import { useMemo, useState, type FormEvent, type ReactNode } from 'react';
import { getRouteApi, Link, useNavigate } from '@tanstack/react-router';
import { ApiRequestError } from '../../api/client';
import { useBlobStores, useCleanupPolicies, useCreateRepository, useFormats, useRepositoriesByFormat } from '../../api/hooks';
import type { Format, RepositoryType, WritePolicy } from '../../api/types';
import { CodeBlock } from '../../components/CodeBlock';
import { IconArrowDown, IconArrowUp, IconClose } from '../../components/icons';
import { PageHeader } from '../../components/PageHeader';
import { Panel } from '../../components/Panel';
import { Pill } from '../../components/Pill';
import { FORMAT_META } from '../../lib/format';
import {
  ALL_FORMATS,
  blobStoreOption,
  blobStoreWritable,
  FORMAT_HINT,
  GROUP_WRITE_FORMATS,
  initialRepoForm,
  isFormat,
  previewUrl,
  REPO_TYPES,
  repositoryYaml,
  toRepositoryInput,
  TYPE_META,
  validateRepoForm,
  WRITE_POLICY_LABEL,
  type AuthKind,
  type ProxyMode,
  type RepoForm,
  type RepoFormErrors,
  type RepoFormField,
} from '../../lib/repositories';

const route = getRouteApi('/app/repositories/new');

const fieldId = (f: RepoFormField) => `repo-${f}`;
const errorId = (f: RepoFormField) => `repo-${f}-error`;

/** Order in which errors are reported and the first invalid field is focused. */
const FIELD_ORDER: RepoFormField[] = [
  'format', 'type', 'name', 'blobStore', 'remoteUrl', 'metadataTtl', 'contentTtl', 'negativeTtl',
  'authUsername', 'authPassword', 'authHeader', 'authToken', 'proxyUrl', 'caBundle', 'members', 'writeMember',
];

function FieldError({ field, errors }: { field: RepoFormField; errors: RepoFormErrors }) {
  const msg = errors[field];
  return msg ? (
    <span id={errorId(field)} className="field__error">
      {msg}
    </span>
  ) : null;
}

const invalidProps = (field: RepoFormField, errors: RepoFormErrors, hintId?: string) => ({
  id: fieldId(field),
  'aria-invalid': errors[field] ? true : undefined,
  'aria-describedby': [errors[field] ? errorId(field) : '', hintId ?? ''].filter(Boolean).join(' ') || undefined,
});

function Step({ n, title, children, id }: { n: number; title: string; children: ReactNode; id: string }) {
  return (
    <Panel className="step" titleId={id} title={
      <>
        <span className="step__n" aria-hidden="true">{n}</span>
        <span className="visually-hidden">Step {n}: </span>
        {title}
      </>
    }>
      {children}
    </Panel>
  );
}

export function RepoCreatePage() {
  const search = route.useSearch();
  const navigate = useNavigate();
  const [form, setForm] = useState<RepoForm>(() => initialRepoForm(search.format, search.type));
  const [nameTouched, setNameTouched] = useState(false);
  const [blurred, setBlurred] = useState<Partial<Record<RepoFormField, boolean>>>({});
  const [submitted, setSubmitted] = useState(false);
  const [candidate, setCandidate] = useState('');

  const formats = useFormats();
  const stores = useBlobStores();
  const policies = useCleanupPolicies();
  const repos = useRepositoriesByFormat(form.type === 'group' ? form.format : undefined);
  const create = useCreateRepository();

  const set = <K extends keyof RepoForm>(key: K, value: RepoForm[K]) => {
    if (create.isError) create.reset(); // the server's answer was about the previous values
    setForm((f) => ({ ...f, [key]: value }));
  };
  const blur = (f: RepoFormField) => () => setBlurred((b) => ({ ...b, [f]: true }));

  const enabledFormats = formats.data ? new Set(formats.data.items.map((f) => f.name)) : null;
  const writableStores = (stores.data?.items ?? []).filter(blobStoreWritable);

  // Derived defaults: the name follows format and type until the user types one;
  // the first writable blob store is chosen until the user picks another.
  const effective: RepoForm = {
    ...form,
    name: nameTouched || !form.format || !form.type ? form.name : `${form.format}-${form.type}`,
    blobStore: form.blobStore || writableStores[0]?.name || '',
  };

  const memberTypes = useMemo(() => new Map((repos.data ?? []).map((r) => [r.name, r.type] as const)), [repos.data]);
  const errors = validateRepoForm(effective, { memberTypes });
  const visible: RepoFormErrors = Object.fromEntries(
    Object.entries(errors).filter(([k]) => submitted || blurred[k as RepoFormField]),
  );
  const input = toRepositoryInput(effective);
  const yaml = repositoryYaml(input);
  const origin = typeof location !== 'undefined' ? location.origin : 'http://localhost';

  const chooseFormat = (f: Format) =>
    setForm((prev) => ({
      ...prev,
      format: f,
      // A remote URL the user did not edit follows the format.
      remoteUrl: prev.format && prev.remoteUrl !== initialRepoForm(prev.format).remoteUrl ? prev.remoteUrl : initialRepoForm(f).remoteUrl,
      writePolicy: initialRepoForm(f).writePolicy,
      members: prev.format === f ? prev.members : [],
      writeMember: prev.format === f ? prev.writeMember : '',
    }));

  const serverError = create.error instanceof ApiRequestError ? create.error : null;
  const nameTaken = serverError?.status === 409;
  const shownErrors: RepoFormErrors = nameTaken && !visible.name
    ? { ...visible, name: 'A repository with this name already exists.' }
    : visible;

  const onSubmit = (e: FormEvent) => {
    e.preventDefault();
    setSubmitted(true);
    const first = FIELD_ORDER.find((f) => errors[f]);
    if (first) {
      const el = document.getElementById(fieldId(first)) ?? document.querySelector<HTMLElement>(`[name="${first}"]`);
      el?.focus();
      return;
    }
    create.mutate(input, {
      onSuccess: (repo) => void navigate({ to: '/repositories', search: { q: repo.name } }),
    });
  };

  const errorCount = Object.keys(errors).length;
  const submitError = serverError
    ? serverError.status === 403
      ? 'You do not have permission to create repositories.'
      : serverError.status === 409
        ? 'A repository with this name already exists. Choose another name.'
        : (serverError.body?.message ?? 'The server could not create the repository.')
    : create.isError
      ? 'Cannot reach the server. Check your connection and try again.'
      : null;

  const candidates = (repos.data ?? []).filter((r) => r.name !== effective.name && !form.members.includes(r.name));
  const hostedMembers = form.members.filter((m) => memberTypes.get(m) === 'hosted');
  const policyOptions = (policies.data?.items ?? []).filter((p) => !p.format || p.format === form.format);

  const moveMember = (i: number, d: -1 | 1) =>
    setForm((f) => {
      const members = [...f.members];
      const j = i + d;
      if (j < 0 || j >= members.length) return f;
      [members[i], members[j]] = [members[j]!, members[i]!];
      return { ...f, members };
    });

  return (
    <div className="screen">
      <PageHeader
        title="Create a repository"
        description="Three short steps. You can change everything later except the name and format."
        actions={
          <Link to="/repositories" className="button button--secondary">
            Cancel
          </Link>
        }
      />

      <form className="repo-create" onSubmit={onSubmit} noValidate aria-label="New repository">
        <div className="repo-create__steps">
          <Step n={1} title="What will it store?" id="step-format">
            <div className="choice-grid choice-grid--formats" role="radiogroup" aria-labelledby="step-format" aria-describedby={shownErrors.format ? errorId('format') : undefined}>
              {ALL_FORMATS.map((f) => {
                const off = enabledFormats !== null && !enabledFormats.has(f);
                return (
                  <label key={f} className={['choice', form.format === f ? 'is-selected' : '', off ? 'is-disabled' : ''].join(' ')}>
                    <input
                      type="radio"
                      name="format"
                      value={f}
                      className="visually-hidden"
                      checked={form.format === f}
                      disabled={off}
                      onChange={() => chooseFormat(f)}
                    />
                    <span className="choice__title">{FORMAT_META[f].label}</span>
                    <span className="choice__hint">{off ? 'Not enabled on this server' : FORMAT_HINT[f]}</span>
                  </label>
                );
              })}
            </div>
            <FieldError field="format" errors={shownErrors} />
          </Step>

          <Step n={2} title="How should it behave?" id="step-type">
            <div className="choice-grid choice-grid--types" role="radiogroup" aria-labelledby="step-type" aria-describedby={shownErrors.type ? errorId('type') : undefined}>
              {REPO_TYPES.map((t) => (
                <label key={t} className={['choice', 'choice--type', form.type === t ? 'is-selected' : ''].join(' ')}>
                  <span className="choice__row">
                    <span className="choice__title">{TYPE_META[t].label}</span>
                    <input type="radio" name="type" value={t} checked={form.type === t} onChange={() => set('type', t as RepositoryType)} />
                  </span>
                  <span className="choice__hint">{TYPE_META[t].explain}</span>
                </label>
              ))}
            </div>
            <FieldError field="type" errors={shownErrors} />
          </Step>

          <Step n={3} title="Settings" id="step-settings">
            <div className="form-grid">
              <div className="field">
                <label htmlFor={fieldId('name')} className="field__label">Name</label>
                <input
                  className="input input--mono"
                  type="text"
                  autoComplete="off"
                  autoCapitalize="off"
                  spellCheck={false}
                  maxLength={63}
                  value={effective.name}
                  onChange={(e) => {
                    setNameTouched(true);
                    set('name', e.target.value);
                  }}
                  onBlur={blur('name')}
                  {...invalidProps('name', shownErrors, 'repo-name-hint')}
                />
                <span id="repo-name-hint" className="field__hint">Lowercase letters, numbers and dashes</span>
                <FieldError field="name" errors={shownErrors} />
              </div>

              {form.type !== 'group' && (
                <div className="field">
                  <label htmlFor={fieldId('blobStore')} className="field__label">Blob store</label>
                  <select
                    className="input"
                    value={effective.blobStore}
                    onChange={(e) => set('blobStore', e.target.value)}
                    onBlur={blur('blobStore')}
                    disabled={stores.isPending}
                    {...invalidProps('blobStore', shownErrors, 'repo-blob-hint')}
                  >
                    {stores.isPending && <option value="">Loading…</option>}
                    {stores.data && writableStores.length === 0 && <option value="">No writable blob store</option>}
                    {(stores.data?.items ?? []).map((b) => (
                      <option key={b.name} value={b.name} disabled={!blobStoreWritable(b)}>
                        {blobStoreOption(b)}
                      </option>
                    ))}
                  </select>
                  <span id="repo-blob-hint" className="field__hint">
                    {stores.isError ? 'Could not load blob stores.' : 'Where the bytes are kept'}
                  </span>
                  <FieldError field="blobStore" errors={shownErrors} />
                </div>
              )}

              <div className="field field--wide">
                <label htmlFor="repo-description" className="field__label">
                  Description <span className="field__optional">optional</span>
                </label>
                <input id="repo-description" className="input" type="text" value={form.description} onChange={(e) => set('description', e.target.value)} />
              </div>

              {form.type === 'hosted' && (
                <div className="field">
                  <label htmlFor="repo-writePolicy" className="field__label">Re-uploads</label>
                  <select id="repo-writePolicy" className="input" value={form.writePolicy} onChange={(e) => set('writePolicy', e.target.value as WritePolicy)}>
                    {(Object.keys(WRITE_POLICY_LABEL) as WritePolicy[]).map((p) => (
                      <option key={p} value={p}>{WRITE_POLICY_LABEL[p]}</option>
                    ))}
                  </select>
                </div>
              )}

              {form.type === 'proxy' && <ProxySettings form={form} set={set} errors={shownErrors} blur={blur} />}

              {form.type === 'group' && (
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
                    <select
                      className="input"
                      value={candidate}
                      onChange={(e) => setCandidate(e.target.value)}
                      disabled={!form.format || repos.isPending}
                      {...invalidProps('members', shownErrors)}
                    >
                      <option value="">
                        {!form.format
                          ? 'Pick a format first'
                          : repos.isPending
                            ? 'Loading repositories…'
                            : candidates.length === 0
                              ? `No other ${FORMAT_META[form.format].label} repositories`
                              : 'Choose a repository…'}
                      </option>
                      {candidates.map((r) => (
                        <option key={r.name} value={r.name}>
                          {r.name} ({TYPE_META[r.type].label.toLowerCase()})
                        </option>
                      ))}
                    </select>
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
                  <FieldError field="members" errors={shownErrors} />
                </div>
              )}

              {form.type === 'group' && form.format && GROUP_WRITE_FORMATS.includes(form.format) && (
                <div className="field">
                  <label htmlFor={fieldId('writeMember')} className="field__label">Publishing through the group</label>
                  <select
                    className="input"
                    value={form.writeMember}
                    onChange={(e) => set('writeMember', e.target.value)}
                    {...invalidProps('writeMember', shownErrors, 'repo-write-hint')}
                  >
                    <option value="">Read-only group</option>
                    {hostedMembers.map((m) => (
                      <option key={m} value={m}>Send uploads to {m}</option>
                    ))}
                  </select>
                  <span id="repo-write-hint" className="field__hint">Only hosted members can receive uploads</span>
                  <FieldError field="writeMember" errors={shownErrors} />
                </div>
              )}

              <div className="field">
                <label htmlFor="repo-cleanup" className="field__label">Cleanup policy</label>
                <select
                  id="repo-cleanup"
                  className="input"
                  value={form.cleanupPolicy}
                  onChange={(e) => set('cleanupPolicy', e.target.value)}
                  disabled={!policies.data}
                  aria-describedby="repo-cleanup-hint"
                >
                  <option value="">Keep everything</option>
                  {policyOptions.map((p) => (
                    <option key={p.name} value={p.name}>{p.description ? `${p.name} · ${p.description}` : p.name}</option>
                  ))}
                </select>
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
          </Step>
        </div>

        <aside className="repo-create__summary" aria-labelledby="summary-title">
          <Panel title="Summary" titleId="summary-title" as="div" className="summary">
            <dl className="summary__list">
              <dt>Format</dt>
              <dd>{form.format && isFormat(form.format) ? FORMAT_META[form.format].label : '—'}</dd>
              <dt>Type</dt>
              <dd>{form.type ? TYPE_META[form.type].label : '—'}</dd>
              <dt>URL</dt>
              <dd className="summary__url">{previewUrl(form.format, effective.name, origin)}</dd>
            </dl>
            <div className="summary__code">
              <p className="summary__label">Same thing as code</p>
              <CodeBlock label="repository configuration as YAML" lines={yaml.trimEnd().split('\n').map((text) => ({ text, comment: text.trimStart().startsWith('#') }))} />
              <p className="field__hint">
                <code className="inline-code">POST /api/v1/repositories</code> accepts this YAML with <code className="inline-code">Content-Type: application/yaml</code>.
              </p>
            </div>
            {submitted && errorCount > 0 && (
              <p className="form-error" role="alert">
                {errorCount === 1 ? 'One setting needs attention.' : `${errorCount} settings need attention.`}
              </p>
            )}
            {submitError && (
              <p className="form-error" role="alert">
                {submitError}
              </p>
            )}
            <button type="submit" className="button button--primary button--block" disabled={create.isPending}>
              {create.isPending ? 'Creating…' : 'Create repository'}
            </button>
          </Panel>
        </aside>
      </form>
    </div>
  );
}

type Setter = <K extends keyof RepoForm>(key: K, value: RepoForm[K]) => void;

function ProxySettings({ form, set, errors, blur }: { form: RepoForm; set: Setter; errors: RepoFormErrors; blur: (f: RepoFormField) => () => void }) {
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
            <select id="repo-authKind" className="input" value={form.authKind} onChange={(e) => set('authKind', e.target.value as AuthKind)}>
              <option value="none">None (public registry)</option>
              <option value="basic">Username and password</option>
              <option value="bearer">Bearer token</option>
              <option value="header">Token in a custom header</option>
            </select>
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
                <input className="input" type="password" autoComplete="new-password" value={form.authPassword} onChange={(e) => set('authPassword', e.target.value)} onBlur={blur('authPassword')} {...invalidProps('authPassword', errors)} />
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
              <input className="input" type="password" autoComplete="new-password" value={form.authToken} onChange={(e) => set('authToken', e.target.value)} onBlur={blur('authToken')} {...invalidProps('authToken', errors)} />
              <FieldError field="authToken" errors={errors} />
            </div>
          )}
        </div>

        <fieldset className="radio-row">
          <legend className="field__label">Outbound proxy</legend>
          {([
            ['inherit', 'Server setting'],
            ['none', 'Direct connection'],
            ['url', 'Through this proxy'],
          ] as [ProxyMode, string][]).map(([v, label]) => (
            <label key={v} className="checkbox">
              <input type="radio" name="proxyMode" value={v} checked={form.proxyMode === v} onChange={() => set('proxyMode', v)} />
              {label}
            </label>
          ))}
        </fieldset>
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
