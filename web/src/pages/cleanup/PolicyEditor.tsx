import { useId, useState, type ReactNode } from 'react';
import { useRepositories } from '../../api/hooks';
import {
  errorText,
  fetchPreviewCsv,
  saveBlob,
  useDeleteCleanupPolicy,
  usePreviewCleanup,
  useRunCleanupPolicy,
  useSaveCleanupPolicy,
} from '../../api/ops';
import type { CleanupCandidate, CleanupPolicy, CleanupPreview } from '../../api/opsTypes';
import { ConfirmDialog } from '../../components/ConfirmDialog';
import { DataTable, type Column } from '../../components/DataTable';
import { IconDownload, IconPlay, IconX } from '../../components/opsIcons';
import { NotAvailable } from '../../components/NotAvailable';
import { Panel } from '../../components/Panel';
import {
  draftFromPolicy,
  draftToInput,
  emptyDraft,
  policyFilename,
  previewHeadline,
  previewKey,
  validateDraft,
  type DraftErrors,
  type PolicyDraft,
  type Toggle,
} from '../../lib/cleanup';
import { scheduleToCron, describeCron } from '../../lib/cron';
import { FORMAT_META, formatBytes } from '../../lib/format';
import { ALL_FORMATS, isFormat } from '../../lib/repositories';
import { formatUtc } from '../../lib/tasks';
import { RunHistory } from './RunHistory';
import { ScheduleField } from './ScheduleField';

type PreviewItem = CleanupCandidate;

interface PolicyEditorProps {
  /** The saved policy; undefined for a new one. */
  policy?: CleanupPolicy;
  canWrite: boolean;
  canDelete: boolean;
  /** Called with the saved name and a sentence to announce. */
  onSaved: (name: string, message: string) => void;
  onDeleted: (message: string) => void;
}

interface ConditionRowProps {
  keyword: string;
  toggle: Toggle;
  onToggle: (t: Toggle) => void;
  before: string;
  after: string;
  inputLabel: string;
  min: number;
  error?: string;
}

/** One criteria sentence: "DELETE ☑ versions published more than [90] days ago". */
function ConditionRow({ keyword, toggle, onToggle, before, after, inputLabel, min, error }: ConditionRowProps) {
  const id = useId();
  return (
    <div className={toggle.on ? 'rule' : 'rule is-off'}>
      <span className="rule__keyword" aria-hidden="true">
        {keyword}
      </span>
      <input
        id={id}
        type="checkbox"
        className="rule__check"
        checked={toggle.on}
        onChange={(e) => onToggle({ ...toggle, on: e.target.checked })}
        aria-label={`Use condition: ${keyword.toLowerCase()} ${before} … ${after}`}
      />
      <span className="rule__text">
        <label htmlFor={id}>{before}</label>
        <input
          className="input input--inline"
          type="number"
          inputMode="numeric"
          min={min}
          value={toggle.value}
          disabled={!toggle.on}
          aria-label={inputLabel}
          aria-invalid={!!error || undefined}
          onChange={(e) => onToggle({ ...toggle, value: e.target.value })}
        />
        <span>{after}</span>
      </span>
      {error && <span className="field__error rule__error">{error}</span>}
    </div>
  );
}

function Errors({ errors, show }: { errors: DraftErrors; show: boolean }) {
  const list = Object.values(errors);
  if (!show || list.length === 0) return null;
  return (
    <div className="form-error" role="alert">
      Fix {list.length === 1 ? 'this' : 'these'} first:
      <ul className="error-list">
        {list.map((e) => (
          <li key={e}>{e}</li>
        ))}
      </ul>
    </div>
  );
}

export function PolicyEditor({ policy, canWrite, canDelete, onSaved, onDeleted }: PolicyEditorProps) {
  const isNew = !policy;
  const [draft, setDraft] = useState<PolicyDraft>(() => (policy ? draftFromPolicy(policy) : emptyDraft()));
  const [showErrors, setShowErrors] = useState(false);
  const [preview, setPreview] = useState<{ key: string; data: CleanupPreview } | null>(null);
  const [confirm, setConfirm] = useState<'run' | 'delete' | null>(null);
  const [csvBusy, setCsvBusy] = useState(false);
  const [csvError, setCsvError] = useState<string | null>(null);
  const ids = { name: useId(), desc: useId(), format: useId(), repo: useId(), globs: useId() };

  const previewM = usePreviewCleanup();
  const saveM = useSaveCleanupPolicy();
  const deleteM = useDeleteCleanupPolicy();
  const runM = useRunCleanupPolicy();
  const repos = useRepositories({ limit: 1000, format: draft.format || undefined });

  const errors = validateDraft(draft);
  const key = previewKey(draft);
  const fresh = !!preview && preview.key === key;
  const stale = !!preview && !fresh;
  const dirty = !policy || JSON.stringify(draftFromPolicy(policy)) !== JSON.stringify(draft);
  const readOnly = !canWrite;

  const update = (patch: Partial<PolicyDraft>) => {
    setDraft((d) => ({ ...d, ...patch }));
  };

  // Preview needs valid criteria and repositories; the name and schedule do not matter.
  const previewErrors: DraftErrors = { ...errors };
  delete previewErrors.name;
  delete previewErrors.schedule;

  const runPreview = () => {
    setShowErrors(true);
    if (Object.keys(previewErrors).length) return;
    const k = key;
    const body = draftToInput({ ...draft, name: errors.name ? 'preview' : draft.name });
    previewM.mutate(body, { onSuccess: (data) => setPreview({ key: k, data }) });
  };

  const save = () => {
    setShowErrors(true);
    if (Object.keys(errors).length || !fresh || !preview) return;
    saveM.mutate(
      { original: policy?.name, body: draftToInput(draft, preview.data.preview_token) },
      {
        onSuccess: (saved) => onSaved(saved.name, isNew ? `Policy ${saved.name} created.` : `Policy ${saved.name} saved.`),
      },
    );
  };

  const exportCsv = async () => {
    setCsvBusy(true);
    setCsvError(null);
    try {
      const blob = await fetchPreviewCsv(draftToInput({ ...draft, name: errors.name ? 'preview' : draft.name }));
      saveBlob(blob, policyFilename(draft.name, new Date()));
    } catch (e) {
      setCsvError(errorText(e, 'Could not export the preview.'));
    } finally {
      setCsvBusy(false);
    }
  };

  const attached = new Set(draft.repositories);
  const candidates = (repos.data?.items ?? []).filter((r) => !attached.has(r.name));
  const cron = scheduleToCron(draft.schedule);
  const scheduleText = cron ? (describeCron(cron) ?? cron) : cron === null ? 'Manual only' : 'Schedule incomplete';

  const columns: Column<PreviewItem>[] = [
    {
      key: 'version',
      header: 'Version',
      className: 'col-first',
      cell: (c) => (
        <code className="preview-version">
          {c.name}
          {c.format === 'docker' ? ':' : '@'}
          {c.version}
        </code>
      ),
    },
    { key: 'repo', header: 'Repository', className: 'muted', cell: (c) => c.repository },
    { key: 'published', header: 'Published', className: 'muted nowrap', cell: (c) => formatUtc(c.published_at).replace(/, \d\d:\d\d UTC$/, '') },
    {
      key: 'download',
      header: 'Last download',
      className: 'muted nowrap',
      cell: (c) => (c.last_downloaded_at ? formatUtc(c.last_downloaded_at).replace(/, \d\d:\d\d UTC$/, '') : 'Never'),
    },
    {
      key: 'size',
      header: 'Size',
      align: 'right',
      className: 'muted nowrap col-last',
      cell: (c) => (typeof c.size_bytes === 'number' ? formatBytes(c.size_bytes) : <NotAvailable reason="size not reported" />),
    },
  ];

  let previewBody: ReactNode;
  if (previewM.isError) {
    previewBody = <p className="form-error">{errorText(previewM.error, 'The preview failed.')}</p>;
  } else if (!preview) {
    previewBody = (
      <p className="meta">
        Preview shows exactly what these conditions select right now. Saving needs a preview of the current conditions.
      </p>
    );
  }

  return (
    <div className="cleanup-editor">
      <Panel
        title={isNew ? 'New policy' : policy.name}
        titleId="policy-editor-title"
        className="policy-panel"
        action={
          <label className="checkbox policy-panel__enabled">
            <input type="checkbox" checked={draft.enabled} disabled={readOnly} onChange={(e) => update({ enabled: e.target.checked })} />
            <span>
              {draft.enabled ? 'Enabled' : 'Paused'} · {scheduleText.charAt(0).toLowerCase() + scheduleText.slice(1)}
            </span>
          </label>
        }
      >
        <form
          className="policy-form"
          noValidate
          onSubmit={(e) => {
            e.preventDefault();
            runPreview();
          }}
        >
          <fieldset className="policy-form__fields" disabled={readOnly}>
            <legend className="visually-hidden">Policy settings</legend>
            <div className="policy-form__grid">
              <div className="field">
                <label htmlFor={ids.name}>Name</label>
                <input
                  id={ids.name}
                  className="input"
                  value={draft.name}
                  readOnly={!isNew}
                  aria-readonly={!isNew || undefined}
                  aria-invalid={(showErrors && !!errors.name) || undefined}
                  aria-describedby={`${ids.name}-hint`}
                  onChange={(e) => update({ name: e.target.value.trim().toLowerCase() })}
                  placeholder="docker-lean"
                  autoComplete="off"
                  spellCheck={false}
                />
                <span className="field__hint" id={`${ids.name}-hint`}>
                  {isNew ? 'Lowercase letters, digits and dashes. It cannot be changed later.' : 'A policy cannot be renamed.'}
                </span>
              </div>
              <div className="field">
                <label htmlFor={ids.format}>Format</label>
                <select
                  id={ids.format}
                  className="input"
                  value={draft.format}
                  onChange={(e) => update({ format: isFormat(e.target.value) ? e.target.value : '' })}
                >
                  <option value="">Any format</option>
                  {ALL_FORMATS.map((f) => (
                    <option key={f} value={f}>
                      {FORMAT_META[f].label}
                    </option>
                  ))}
                </select>
              </div>
              <div className="field policy-form__wide">
                <label htmlFor={ids.desc}>
                  Description <span className="field__optional">(optional)</span>
                </label>
                <input
                  id={ids.desc}
                  className="input"
                  value={draft.description}
                  onChange={(e) => update({ description: e.target.value })}
                  placeholder="Docker images · keep it lean"
                />
              </div>
            </div>

            <div className="rules" role="group" aria-labelledby="rules-title">
              <h3 className="section-label" id="rules-title">
                Conditions <span className="field__optional">all checked conditions must hold; keep rules always win</span>
              </h3>
              <ConditionRow
                keyword="Delete"
                toggle={draft.olderThan}
                onToggle={(t) => update({ olderThan: t })}
                before="versions published more than"
                after="days ago"
                inputLabel="Days since published"
                min={1}
                error={showErrors ? errors.olderThan : undefined}
              />
              <ConditionRow
                keyword="And"
                toggle={draft.notDownloaded}
                onToggle={(t) => update({ notDownloaded: t })}
                before="not downloaded in the last"
                after="days"
                inputLabel="Days without a download"
                min={1}
                error={showErrors ? errors.notDownloaded : undefined}
              />
              <div className={draft.prereleaseOnly ? 'rule' : 'rule is-off'}>
                <span className="rule__keyword" aria-hidden="true">
                  Only
                </span>
                <input
                  id={`${ids.globs}-pre`}
                  type="checkbox"
                  className="rule__check"
                  checked={draft.prereleaseOnly}
                  onChange={(e) => update({ prereleaseOnly: e.target.checked })}
                />
                <label className="rule__text" htmlFor={`${ids.globs}-pre`}>
                  pre-release and snapshot versions
                </label>
              </div>
              <ConditionRow
                keyword="Keep"
                toggle={draft.keepNewest}
                onToggle={(t) => update({ keepNewest: t })}
                before="the newest"
                after="versions of each package"
                inputLabel="Newest versions to keep"
                min={0}
                error={showErrors ? errors.keepNewest : undefined}
              />
              <div className="rule">
                <span className="rule__keyword" aria-hidden="true">
                  Keep
                </span>
                <span className="rule__check" aria-hidden="true" />
                <span className="rule__text rule__text--grow">
                  <label htmlFor={ids.globs}>tags matching</label>
                  <input
                    id={ids.globs}
                    className="input input--mono rule__globs"
                    value={draft.keepMatching}
                    onChange={(e) => update({ keepMatching: e.target.value })}
                    placeholder="release-*, latest, v*.*.*"
                    spellCheck={false}
                    autoComplete="off"
                    aria-describedby={`${ids.globs}-hint`}
                  />
                </span>
                <span className="field__hint rule__error" id={`${ids.globs}-hint`}>
                  Comma-separated globs; matching versions are never deleted.
                </span>
              </div>
              {showErrors && errors.criteria && <p className="field__error">{errors.criteria}</p>}
            </div>

            <div className="applies" role="group" aria-labelledby="applies-title">
              <h3 className="section-label" id="applies-title">
                Applies to
              </h3>
              <ul className="chips">
                {draft.repositories.map((r) => (
                  <li key={r} className="chip">
                    {r}
                    {!readOnly && (
                      <button
                        type="button"
                        className="chip__remove"
                        aria-label={`Remove ${r}`}
                        onClick={() => update({ repositories: draft.repositories.filter((x) => x !== r) })}
                      >
                        <IconX size={14} />
                      </button>
                    )}
                  </li>
                ))}
                {!readOnly && (
                  <li className="chip chip--add">
                    <label htmlFor={ids.repo} className="visually-hidden">
                      Add repository
                    </label>
                    <select
                      id={ids.repo}
                      className="chip__select"
                      value=""
                      onChange={(e) => e.target.value && update({ repositories: [...draft.repositories, e.target.value] })}
                      aria-invalid={(showErrors && !!errors.repositories) || undefined}
                    >
                      <option value="">{repos.isPending ? 'Loading repositories…' : 'Add repository'}</option>
                      {candidates.map((r) => (
                        <option key={r.id} value={r.name}>
                          {r.name} ({r.type})
                        </option>
                      ))}
                    </select>
                  </li>
                )}
              </ul>
              {repos.isError && <p className="field__error">Could not load repositories.</p>}
              {showErrors && errors.repositories && <p className="field__error">{errors.repositories}</p>}
            </div>

            <ScheduleField value={draft.schedule} onChange={(schedule) => update({ schedule })} error={showErrors ? errors.schedule : undefined} />
          </fieldset>

          <Errors errors={previewErrors} show={showErrors && !fresh} />

          {canWrite && (
            <div className="policy-form__actions">
              <button type="submit" className="button button--secondary" disabled={previewM.isPending} aria-busy={previewM.isPending || undefined}>
                {previewM.isPending ? 'Previewing…' : fresh ? 'Preview again' : 'Preview'}
              </button>
              {!isNew && (
                <button
                  type="button"
                  className="button button--secondary"
                  onClick={() => setConfirm('run')}
                  disabled={dirty}
                  title={dirty ? 'Save your changes before running' : undefined}
                >
                  <IconPlay size={16} /> Run now
                </button>
              )}
              {!isNew && canDelete && (
                <button type="button" className="button button--ghost-danger" onClick={() => setConfirm('delete')}>
                  Delete policy
                </button>
              )}
            </div>
          )}
          {!canWrite && <p className="meta">You can view this policy. Changing it needs the cleanup write privilege.</p>}
          {runM.isSuccess && !dirty && (
            <p className="form-success">Cleanup started. Progress appears in the run history below and under Scheduled tasks.</p>
          )}
        </form>
      </Panel>

      <Panel flush className={['preview-panel', fresh ? 'is-fresh' : ''].join(' ')} aria-labelledby="preview-title">
        <div className="preview-panel__head">
          <div>
            <p className="preview-panel__eyebrow">{stale ? 'Preview · out of date, conditions changed' : 'Preview · nothing is deleted yet'}</p>
            <h2 className="preview-panel__title" id="preview-title" aria-live="polite">
              {preview ? previewHeadline(preview.data.component_count, preview.data.bytes) : 'What would be deleted'}
            </h2>
          </div>
          <div className="preview-panel__actions">
            {preview && (
              <button type="button" className="button button--secondary" onClick={() => void exportCsv()} disabled={csvBusy || stale}>
                <IconDownload size={16} /> {csvBusy ? 'Exporting…' : 'Export CSV'}
              </button>
            )}
            {canWrite && (
              <button
                type="button"
                className="button button--primary"
                onClick={save}
                disabled={!fresh || saveM.isPending || (!dirty && !isNew)}
                aria-describedby="save-hint"
              >
                {saveM.isPending ? 'Saving…' : isNew ? 'Create policy' : 'Save policy'}
              </button>
            )}
          </div>
        </div>
        {canWrite && (
          <p className="preview-panel__hint meta" id="save-hint">
            {!fresh ? 'Run a preview of the current conditions to enable saving.' : !dirty && !isNew ? 'No unsaved changes.' : 'Saving uses this preview.'}
          </p>
        )}
        {csvError && <p className="form-error preview-panel__error">{csvError}</p>}
        {saveM.isError && <p className="form-error preview-panel__error">{errorText(saveM.error, 'The policy could not be saved.')}</p>}
        {previewBody && <div className="preview-panel__body">{previewBody}</div>}
        {preview && (
          <>
            <DataTable
              caption="Versions this policy would delete"
              columns={columns}
              rows={preview.data.sample}
              rowKey={(c) => c.id}
              minWidth={720}
              empty="No version matches these conditions."
            />
            {preview.data.component_count > preview.data.sample.length && (
              <p className="meta preview-panel__foot">
                Showing {preview.data.sample.length} of {new Intl.NumberFormat('en-US').format(preview.data.component_count)}. Export CSV for the full list.
              </p>
            )}
          </>
        )}
      </Panel>

      {policy && <RunHistory policy={policy.name} />}

      {confirm === 'run' && policy && (
        <ConfirmDialog
          title={`Run ${policy.name} now?`}
          confirmLabel="Run cleanup now"
          tone="danger"
          busy={runM.isPending}
          error={runM.isError ? errorText(runM.error, 'The run could not be started.') : null}
          onCancel={() => setConfirm(null)}
          onConfirm={() => runM.mutate(policy.name, { onSuccess: () => setConfirm(null) })}
        >
          <p>
            Deletes every version its conditions select right now in {policy.repositories.length}{' '}
            {policy.repositories.length === 1 ? 'repository' : 'repositories'} ({policy.repositories.join(', ')}).
          </p>
          {fresh && preview ? (
            <p>
              Last preview: <strong>{previewHeadline(preview.data.component_count, preview.data.bytes)}</strong>.
            </p>
          ) : (
            <p>Preview first if you are not sure what it selects.</p>
          )}
          <p className="meta">Deleted versions go to soft-delete; blob compaction frees the space later.</p>
        </ConfirmDialog>
      )}
      {confirm === 'delete' && policy && (
        <ConfirmDialog
          title={`Delete policy ${policy.name}?`}
          confirmLabel="Delete policy"
          tone="danger"
          busy={deleteM.isPending}
          error={deleteM.isError ? errorText(deleteM.error, 'The policy could not be deleted.') : null}
          onCancel={() => setConfirm(null)}
          onConfirm={() =>
            deleteM.mutate(policy.name, {
              onSuccess: () => {
                setConfirm(null);
                onDeleted(`Policy ${policy.name} deleted.`);
              },
            })
          }
        >
          <p>
            The policy stops running and is detached from {policy.repositories.join(', ')}. Packages it already deleted are not restored.
          </p>
        </ConfirmDialog>
      )}
    </div>
  );
}
