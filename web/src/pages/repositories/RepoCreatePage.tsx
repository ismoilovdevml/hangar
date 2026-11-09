import { useMemo, useState, type FormEvent, type ReactNode } from 'react';
import { getRouteApi, Link, useNavigate } from '@tanstack/react-router';
import { ApiRequestError } from '../../api/client';
import { useBlobStores, useCreateRepository, useFormats, useRepositoriesByFormat } from '../../api/hooks';
import type { Format, RepositoryType } from '../../api/types';
import { CodeBlock } from '../../components/CodeBlock';
import { PageHeader } from '../../components/PageHeader';
import { Panel } from '../../components/Panel';
import { FormatIcon, FormatName } from '../../components/FormatIcon';
import { Select } from '../../components/Select';
import { FORMAT_META } from '../../lib/format';
import { isForbidden } from '../../lib/gates';
import {
  ALL_FORMATS,
  blobStoreOption,
  blobStoreWritable,
  FORMAT_HINT,
  initialRepoForm,
  isFormat,
  previewUrl,
  REPO_TYPES,
  repositoryYaml,
  toRepositoryInput,
  TYPE_META,
  validateRepoForm,
  type RepoForm,
  type RepoFormErrors,
  type RepoFormField,
} from '../../lib/repositories';
import { FieldError, RepoSettingsFields } from './RepoFormFields';
import { errorId, focusFirstError, invalidProps } from './repoFormHelpers';

const route = getRouteApi('/app/repositories/new');

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

  const formats = useFormats();
  const stores = useBlobStores();
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
  // Until a format and a type are chosen the preview says so, rather than showing the defaults the request would fall back to.
  const yaml = repositoryYaml(input, { format: !!form.format, type: !!form.type });
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
    if (focusFirstError(errors)) return;
    create.mutate(input, {
      // The repository's page, where the next step (connecting a client) is waiting.
      onSuccess: (repo) => void navigate({ to: '/repositories/$name/settings', params: { name: repo.name }, state: { created: repo.name } }),
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

  const identity = (
    <>
      <div className="field">
        <label htmlFor="repo-name" className="field__label">Name</label>
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
          <label htmlFor="repo-blobStore" className="field__label">Blob store</label>
          <Select
            value={effective.blobStore}
            onChange={(v) => set('blobStore', v)}
            onBlur={blur('blobStore')}
            disabled={stores.isPending}
            placeholder={stores.isPending ? 'Loading…' : writableStores.length === 0 ? 'No writable blob store' : 'Choose a blob store'}
            options={(stores.data?.items ?? []).map((b) => ({ value: b.name, label: blobStoreOption(b), disabled: !blobStoreWritable(b) }))}
            {...invalidProps('blobStore', shownErrors, 'repo-blob-hint')}
          />
          <span id="repo-blob-hint" className="field__hint">
            {stores.isError
              ? isForbidden(stores.error)
                ? 'Listing blob stores needs the system:blobstores:read privilege.'
                : 'Could not load blob stores.'
              : 'Where the bytes are kept'}
          </span>
          <FieldError field="blobStore" errors={shownErrors} />
        </div>
      )}
    </>
  );

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
                    <span className="choice__title choice__title--format">
                      <FormatIcon format={f} size={20} label={null} tone={off ? 'current' : 'brand'} />
                      {FORMAT_META[f].label}
                    </span>
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
            <RepoSettingsFields
              form={form}
              selfName={effective.name}
              set={set}
              setForm={setForm}
              errors={shownErrors}
              blur={blur}
              identity={identity}
            />
          </Step>
        </div>

        <aside className="repo-create__summary" aria-labelledby="summary-title">
          <Panel title="Summary" titleId="summary-title" as="div" className="summary">
            <dl className="summary__list">
              <dt>Format</dt>
              <dd>{form.format && isFormat(form.format) ? <FormatName format={form.format} /> : '—'}</dd>
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
