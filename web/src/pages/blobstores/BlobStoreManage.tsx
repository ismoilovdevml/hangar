import { useState, type FormEvent } from 'react';
import { useNavigate } from '@tanstack/react-router';
import { ApiRequestError } from '../../api/client';
import { useDeleteBlobStore, useUpdateBlobStore } from '../../api/catalog';
import { usePermissions } from '../../api/permissions';
import type { BlobStore } from '../../api/types';
import { ConfirmDialog } from '../../components/ConfirmDialog';
import { Panel } from '../../components/Panel';
import { Select } from '../../components/Select';
import { QUOTA_UNITS, quotaBytes, quotaToForm, type QuotaUnit } from '../../lib/blobStores';
import { formatBytes, formatCompact } from '../../lib/format';

function message(err: unknown, action: string): string {
  if (err instanceof ApiRequestError) {
    if (err.status === 403) return `You do not have permission to ${action} blob stores.`;
    return err.body?.message ?? `The server could not ${action} the blob store.`;
  }
  return 'Cannot reach the server. Check your connection and try again.';
}

/** Soft quota editor and deletion. Location and credentials are fixed once a store holds data. */
export function BlobStoreManage({ s }: { s: BlobStore }) {
  const navigate = useNavigate();
  const canAdmin = usePermissions().can('admin', 'system', 'blobstores');
  const initial = quotaToForm(s.soft_quota_bytes);
  const [quota, setQuota] = useState(initial.quota);
  const [unit, setUnit] = useState<QuotaUnit>(initial.quotaUnit);
  const [saved, setSaved] = useState(false);
  const [confirm, setConfirm] = useState(false);
  const update = useUpdateBlobStore(s.name);
  const remove = useDeleteBlobStore(s.name);

  const bytes = quotaBytes(quota, unit);
  const invalid = bytes !== null && Number.isNaN(bytes);
  const changed = (bytes ?? null) !== (s.soft_quota_bytes ?? null);
  const inUse = s.repository_count > 0 || s.blob_count > 0;

  const onSubmit = (e: FormEvent) => {
    e.preventDefault();
    if (invalid || !changed) return;
    // Only the quota is sent: left-out config sections keep their stored values.
    update.mutate(bytes === null ? { name: s.name, kind: s.kind } : { name: s.name, kind: s.kind, soft_quota_bytes: bytes }, {
      onSuccess: () => setSaved(true),
    });
  };

  return (
    <Panel title="Manage" titleId="manage-title" className="store-manage">
      <form className="store-manage__quota" onSubmit={onSubmit} noValidate aria-label="Change soft quota">
        <div className="field">
          <label htmlFor="manage-quota" className="field__label">
            Soft quota
          </label>
          <div className="quota-input">
            <input
              id="manage-quota"
              className="input"
              inputMode="decimal"
              autoComplete="off"
              placeholder="No quota"
              value={quota}
              disabled={!canAdmin}
              aria-invalid={invalid || undefined}
              aria-describedby="manage-quota-hint"
              onChange={(e) => {
                setSaved(false);
                update.reset();
                setQuota(e.target.value);
              }}
            />
            <label htmlFor="manage-quota-unit" className="visually-hidden">
              Quota unit
            </label>
            <Select<QuotaUnit> id="manage-quota-unit" value={unit} disabled={!canAdmin} options={QUOTA_UNITS} onChange={setUnit} />
          </div>
          <span id="manage-quota-hint" className={invalid ? 'field__error' : 'field__hint'}>
            {invalid ? 'Enter a positive number, or leave empty for no quota.' : 'A warning threshold, not a hard limit. Empty removes it.'}
          </span>
        </div>
        <button type="submit" className="button button--secondary" disabled={!canAdmin || invalid || !changed || update.isPending}>
          {update.isPending ? 'Saving…' : saved && !changed ? 'Saved' : 'Save quota'}
        </button>
        {update.isError && (
          <p className="form-error" role="alert">
            {message(update.error, 'change')}
          </p>
        )}
      </form>

      <div className="store-manage__delete">
        <p className="danger-zone__text">
          {inUse
            ? `Deleting needs an empty store: it still holds ${s.repository_count} ${s.repository_count === 1 ? 'repository' : 'repositories'} and ${formatCompact(s.blob_count)} blobs (${formatBytes(s.used_bytes)}).`
            : 'The store holds no repositories and no blobs. Deleting it removes the definition; files on the backend are not touched.'}
        </p>
        <div>
          <button
            type="button"
            className="button button--danger-outline"
            disabled={!canAdmin || inUse}
            title={!canAdmin ? 'Needs the blob store admin privilege' : undefined}
            onClick={() => {
              remove.reset();
              setConfirm(true);
            }}
          >
            Delete {s.name}
          </button>
        </div>
      </div>

      {confirm && (
        <ConfirmDialog
          title={`Delete ${s.name}?`}
          confirmLabel="Delete blob store"
          tone="danger"
          typeToConfirm={s.name}
          busy={remove.isPending}
          error={remove.isError ? message(remove.error, 'delete') : null}
          onCancel={() => setConfirm(false)}
          onConfirm={() => remove.mutate(undefined, { onSuccess: () => void navigate({ to: '/blob-stores' }) })}
        >
          <p>New repositories can no longer choose it. This cannot be undone.</p>
        </ConfirmDialog>
      )}
    </Panel>
  );
}
