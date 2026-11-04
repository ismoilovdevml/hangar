import { useState, type FormEvent } from 'react';
import { useNavigate } from '@tanstack/react-router';
import { ApiRequestError } from '../../api/client';
import { useCreateBlobStore } from '../../api/catalog';
import { Panel } from '../../components/Panel';
import { Segmented } from '../../components/Segmented';
import {
  initialBlobStoreForm,
  toBlobStoreInput,
  validateBlobStoreForm,
  type BlobStoreErrors,
  type BlobStoreField,
  type BlobStoreForm,
  type QuotaUnit,
  type StoreKind,
} from '../../lib/blobStores';

const FIELD_ORDER: BlobStoreField[] = ['name', 'quota', 'path', 'endpoint', 'bucket', 'accessKeyId', 'secretAccessKey'];
const id = (f: BlobStoreField) => `store-${f}`;
const errId = (f: BlobStoreField) => `store-${f}-error`;

function submitMessage(err: unknown): string {
  if (err instanceof ApiRequestError) {
    if (err.status === 403) return 'You do not have permission to add blob stores.';
    if (err.status === 409) return 'A blob store with this name already exists.';
    return err.body?.message ?? 'The server could not add the blob store.';
  }
  return 'Cannot reach the server. Check your connection and try again.';
}

/** Form for a new filesystem or S3 store. The secret key is write-only: sent once, never shown again. */
export function BlobStoreCreate({ onClose }: { onClose: () => void }) {
  const navigate = useNavigate();
  const [form, setForm] = useState<BlobStoreForm>(initialBlobStoreForm);
  const [submitted, setSubmitted] = useState(false);
  const create = useCreateBlobStore();
  const errors = validateBlobStoreForm(form);
  const shown: BlobStoreErrors = submitted ? errors : {};

  const set = <K extends keyof BlobStoreForm>(k: K, v: BlobStoreForm[K]) => {
    if (create.isError) create.reset();
    setForm((f) => ({ ...f, [k]: v }));
  };

  const onSubmit = (e: FormEvent) => {
    e.preventDefault();
    setSubmitted(true);
    const first = FIELD_ORDER.find((f) => errors[f]);
    if (first) {
      document.getElementById(id(first))?.focus();
      return;
    }
    create.mutate(toBlobStoreInput(form), {
      onSuccess: (s) => void navigate({ to: '/blob-stores/$name', params: { name: s.name } }),
    });
  };

  const field = (f: BlobStoreField, label: string, input: JSX.Element, hint?: string, wide = false) => (
    <div className={wide ? 'field field--wide' : 'field'}>
      <label htmlFor={id(f)} className="field__label">
        {label}
      </label>
      {input}
      {hint && (
        <span id={`${id(f)}-hint`} className="field__hint">
          {hint}
        </span>
      )}
      {shown[f] && (
        <span id={errId(f)} className="field__error">
          {shown[f]}
        </span>
      )}
    </div>
  );
  const a11y = (f: BlobStoreField, hint = false) => ({
    id: id(f),
    'aria-invalid': shown[f] ? true : undefined,
    'aria-describedby': [shown[f] ? errId(f) : '', hint ? `${id(f)}-hint` : ''].filter(Boolean).join(' ') || undefined,
  });
  const text = (f: 'name' | 'path' | 'endpoint' | 'bucket' | 'accessKeyId', placeholder?: string, hint = false) => (
    <input
      className="input input--mono"
      type="text"
      autoComplete="off"
      autoCapitalize="off"
      spellCheck={false}
      placeholder={placeholder}
      value={form[f]}
      onChange={(e) => set(f, e.target.value)}
      {...a11y(f, hint)}
    />
  );

  return (
    <Panel title="Add a blob store" titleId="add-store-title" className="store-create">
      <form onSubmit={onSubmit} noValidate aria-labelledby="add-store-title" className="store-create__form">
        <Segmented<StoreKind>
          label="Storage kind"
          options={[
            { value: 'fs', label: 'Filesystem' },
            { value: 's3', label: 'S3 / MinIO' },
          ]}
          value={form.kind}
          onChange={(k) => set('kind', k)}
          className="store-create__kind"
        />
        <div className="form-grid">
          {field('name', 'Name', text('name', 'e.g. s3-eu-primary', true), 'Lowercase letters, numbers and dashes. Cannot be changed later.')}
          {field(
            'quota',
            'Soft quota',
            <div className="quota-input">
              <input
                className="input"
                type="text"
                inputMode="decimal"
                autoComplete="off"
                placeholder="No quota"
                value={form.quota}
                onChange={(e) => set('quota', e.target.value)}
                {...a11y('quota', true)}
              />
              <label htmlFor="store-quota-unit" className="visually-hidden">
                Quota unit
              </label>
              <select id="store-quota-unit" className="input" value={form.quotaUnit} onChange={(e) => set('quotaUnit', e.target.value as QuotaUnit)}>
                <option value="GB">GB</option>
                <option value="TB">TB</option>
              </select>
            </div>,
            'A warning threshold, not a hard limit.',
          )}
          {form.kind === 'fs' ? (
            field('path', 'Directory on the server', text('path', '/var/lib/hangar/blobs', true), 'Absolute path; the hangar process needs write access.', true)
          ) : (
            <>
              {field('bucket', 'Bucket', text('bucket', 'acme-artifacts'))}
              <div className="field">
                <label htmlFor="store-prefix" className="field__label">
                  Prefix <span className="field__optional">optional</span>
                </label>
                <input id="store-prefix" className="input input--mono" type="text" autoComplete="off" spellCheck={false} placeholder="hangar/" value={form.prefix} onChange={(e) => set('prefix', e.target.value)} />
              </div>
              {field('endpoint', 'Endpoint', text('endpoint', 'https://minio.internal:9000', true), 'Leave empty for AWS S3.')}
              <div className="field">
                <label htmlFor="store-region" className="field__label">
                  Region <span className="field__optional">optional</span>
                </label>
                <input id="store-region" className="input input--mono" type="text" autoComplete="off" spellCheck={false} placeholder="eu-central-1" value={form.region} onChange={(e) => set('region', e.target.value)} />
              </div>
              {field('accessKeyId', 'Access key id', text('accessKeyId', undefined, true), 'Leave both keys empty to use the environment or instance credentials.')}
              {field(
                'secretAccessKey',
                'Secret access key',
                <input
                  className="input"
                  type="password"
                  autoComplete="new-password"
                  value={form.secretAccessKey}
                  onChange={(e) => set('secretAccessKey', e.target.value)}
                  {...a11y('secretAccessKey', true)}
                />,
                'Write-only: stored encrypted and never shown again.',
              )}
              <label className="checkbox field--wide">
                <input type="checkbox" checked={form.usePathStyle} onChange={(e) => set('usePathStyle', e.target.checked)} />
                Path-style addressing (MinIO and most S3-compatible servers)
              </label>
            </>
          )}
        </div>
        {submitted && Object.keys(errors).length > 0 && (
          <p className="form-error" role="alert">
            {Object.keys(errors).length === 1 ? 'One setting needs attention.' : `${Object.keys(errors).length} settings need attention.`}
          </p>
        )}
        {create.isError && (
          <p className="form-error" role="alert">
            {submitMessage(create.error)}
          </p>
        )}
        <div className="store-create__actions">
          <button type="button" className="button button--secondary" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="button button--primary" disabled={create.isPending}>
            {create.isPending ? 'Adding…' : 'Add blob store'}
          </button>
        </div>
      </form>
    </Panel>
  );
}
