import { useState, type FormEvent } from 'react';
import { ApiRequestError } from '../../api/client';
import { useUpdateBlobStore } from '../../api/catalog';
import type { BlobStore, BlobStoreInput } from '../../api/types';
import { TestOutcome } from './TestOutcome';
import { useConnectionTest } from './useConnectionTest';

function message(err: unknown): string {
  if (err instanceof ApiRequestError) {
    if (err.status === 403) return 'You do not have permission to change blob stores.';
    return err.body?.message ?? 'The server could not save the connection.';
  }
  return 'Cannot reach the server. Check your connection and try again.';
}

interface Draft {
  endpoint: string;
  bucket: string;
  prefix: string;
  region: string;
  accessKeyId: string;
  secret: string;
  usePathStyle: boolean;
}

const draftOf = (s: BlobStore): Draft => ({
  endpoint: s.s3?.endpoint ?? '',
  bucket: s.s3?.bucket ?? '',
  prefix: s.s3?.prefix ?? '',
  region: s.s3?.region ?? '',
  accessKeyId: s.s3?.access_key_id ?? '',
  secret: '',
  usePathStyle: !!s.s3?.use_path_style,
});

/**
 * Connection settings of an S3 store added through the UI: credentials,
 * region and path style can always change; endpoint, bucket and prefix only
 * while the store holds nothing (its content would no longer be found).
 */
export function BlobStoreConnection({ s }: { s: BlobStore }) {
  const [open, setOpen] = useState(false);
  const [d, setD] = useState<Draft>(() => draftOf(s));
  const update = useUpdateBlobStore(s.name);
  const check = useConnectionTest();
  const locked = s.repository_count > 0 || s.blob_count > 0;
  const keyChanged = d.accessKeyId.trim() !== (s.s3?.access_key_id ?? '');
  const secretMissing = !!d.accessKeyId.trim() && keyChanged && !d.secret;

  const set = <K extends keyof Draft>(k: K, v: Draft[K]) => {
    update.reset();
    setD((x) => ({ ...x, [k]: v }));
  };

  const input: BlobStoreInput = {
    name: s.name,
    kind: 's3',
    // PUT without a quota removes it: send the current one back.
    ...(s.soft_quota_bytes ? { soft_quota_bytes: s.soft_quota_bytes } : {}),
    s3: {
      bucket: d.bucket.trim(),
      use_path_style: d.usePathStyle,
      ...(d.endpoint.trim() ? { endpoint: d.endpoint.trim() } : {}),
      ...(d.prefix.trim() ? { prefix: d.prefix.trim() } : {}),
      ...(d.region.trim() ? { region: d.region.trim() } : {}),
      ...(d.accessKeyId.trim() ? { access_key_id: d.accessKeyId.trim() } : {}),
      // Left out, the stored secret stays (with the same access key).
      ...(d.accessKeyId.trim() && d.secret ? { secret_access_key: d.secret } : {}),
    },
  };
  const tested = check.resultFor(input);
  const invalid = !d.bucket.trim() || secretMissing;

  const onSubmit = (e: FormEvent) => {
    e.preventDefault();
    if (invalid || update.isPending) return;
    check.submit(input, () =>
      update.mutate(input, {
        onSuccess: (next) => {
          setOpen(false);
          setD(draftOf(next));
          check.reset();
        },
      }),
    );
  };

  if (!open) {
    return (
      <div className="store-connection">
        <p className="danger-zone__text">Access key, secret, region and addressing of this S3 store. The secret is write-only.</p>
        <div>
          <button type="button" className="button button--secondary" onClick={() => setOpen(true)}>
            Edit connection
          </button>
        </div>
      </div>
    );
  }

  const text = (k: 'endpoint' | 'bucket' | 'prefix' | 'region' | 'accessKeyId', label: string, opts: { disabled?: boolean; placeholder?: string } = {}) => (
    <div className="field">
      <label htmlFor={`conn-${k}`} className="field__label">
        {label}
      </label>
      <input
        id={`conn-${k}`}
        className="input input--mono"
        type="text"
        autoComplete="off"
        autoCapitalize="off"
        spellCheck={false}
        disabled={opts.disabled}
        placeholder={opts.placeholder}
        value={d[k]}
        onChange={(e) => set(k, e.target.value)}
      />
    </div>
  );

  return (
    <form className="store-connection store-connection__form" onSubmit={onSubmit} noValidate aria-label={`Connection of ${s.name}`}>
      <div className="form-grid">
        {text('endpoint', 'Endpoint', { disabled: locked, placeholder: 'AWS S3' })}
        {text('bucket', 'Bucket', { disabled: locked })}
        {text('prefix', 'Prefix', { disabled: locked })}
        {text('region', 'Region', { placeholder: 'us-east-1' })}
        {text('accessKeyId', 'Access key id', { placeholder: 'From the environment' })}
        <div className="field">
          <label htmlFor="conn-secret" className="field__label">
            Secret access key
          </label>
          <input
            id="conn-secret"
            className="input"
            type="password"
            autoComplete="new-password"
            value={d.secret}
            placeholder={s.s3?.access_key_id && !keyChanged ? 'Leave empty to keep the stored secret' : undefined}
            aria-invalid={secretMissing || undefined}
            aria-describedby="conn-secret-hint"
            onChange={(e) => set('secret', e.target.value)}
          />
          <span id="conn-secret-hint" className={secretMissing ? 'field__error' : 'field__hint'}>
            {secretMissing ? 'A new access key needs its secret.' : 'Write-only: stored encrypted and never shown again.'}
          </span>
        </div>
        <label className="checkbox field--wide">
          <input type="checkbox" checked={d.usePathStyle} onChange={(e) => set('usePathStyle', e.target.checked)} />
          Path-style addressing (MinIO and most S3-compatible servers)
        </label>
      </div>
      {locked && (
        <p className="field__hint">
          Endpoint, bucket and prefix are fixed while the store holds repositories or blobs: existing content would no longer be found.
        </p>
      )}
      <TestOutcome result={tested} pending={check.pending} error={check.error} />
      {update.isError && (
        <p className="form-error" role="alert">
          {message(update.error)}
        </p>
      )}
      <div className="store-connection__actions">
        <button
          type="button"
          className="button button--secondary"
          onClick={() => {
            setOpen(false);
            setD(draftOf(s));
            check.reset();
            update.reset();
          }}
        >
          Cancel
        </button>
        <button type="button" className="button button--secondary" disabled={invalid || check.pending} onClick={() => check.run(input)}>
          Test connection
        </button>
        <button type="submit" className="button button--primary" disabled={invalid || check.pending || update.isPending}>
          {update.isPending ? 'Saving…' : tested && !tested.ok ? 'Save anyway' : 'Save connection'}
        </button>
      </div>
    </form>
  );
}
