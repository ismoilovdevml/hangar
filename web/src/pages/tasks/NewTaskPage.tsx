import { useEffect, useId, useMemo, useRef, useState, type FormEvent } from 'react';
import { PickerLoadError } from '../../components/States';
import { getRouteApi, Link } from '@tanstack/react-router';
import { useCanSystem, useCreateTask, useParseLockfile } from '../../api/ops';
import { errorText } from '../../lib/apiError';
import type { ImportTaskConfig, PrewarmTaskConfig } from '../../api/opsTypes';
import { useBlobStores, useRepositories } from '../../api/hooks';
import type { Repository } from '../../api/types';
import { PageHeader } from '../../components/PageHeader';
import { Panel } from '../../components/Panel';
import { Segmented } from '../../components/Segmented';
import { Select } from '../../components/Select';
import { cronToSchedule, scheduleToCron, type ScheduleDraft } from '../../lib/cron';
import { importHints, splitList, TASK_NAME_RE, type ImportKind, type MaintenanceType, type NewTaskKind } from '../../lib/imports';
import { useDetailsOpen } from '../../lib/useDetailsOpen';
import { ScheduleField } from '../cleanup/ScheduleField';

const route = getRouteApi('/app/tasks/new');

export interface NewTaskSearch {
  kind?: NewTaskKind;
  repository?: string;
}

interface LockfileChip {
  name: string;
  entries: number;
  skipped: number;
}

interface Draft {
  name: string;
  repository: string;
  url: string;
  username: string;
  secret: string;
  secretKind: 'password' | 'token';
  packages: string;
  exclude: string;
  versions: string;
  latest: string;
  prereleases: boolean;
  distribution: string;
  components: string;
  architectures: string;
  files: string;
  coordinates: string;
  inventory: boolean;
  rps: string;
  concurrency: string;
  runNow: boolean;
  schedule: ScheduleDraft;
  maintenance: MaintenanceType;
  /** blob_compaction: one store; empty = all. */
  blobStore: string;
  /** docker_gc: one repository; empty = all Docker repositories. */
  gcRepository: string;
  olderThanDays: string;
  dryRun: boolean;
}

const EMPTY: Draft = {
  name: '',
  repository: '',
  url: '',
  username: '',
  secret: '',
  secretKind: 'password',
  packages: '',
  exclude: '',
  versions: '',
  latest: '',
  prereleases: false,
  distribution: '',
  components: '',
  architectures: '',
  files: '',
  coordinates: '',
  inventory: false,
  rps: '',
  concurrency: '',
  runNow: true,
  schedule: cronToSchedule(null),
  maintenance: 'blob_compaction',
  blobStore: '',
  gcRepository: '',
  olderThanDays: '',
  dryRun: false,
};

const MAINTENANCE: { value: MaintenanceType; label: string; hint: string }[] = [
  { value: 'blob_compaction', label: 'Blob compaction', hint: 'Frees the space of soft-deleted blobs' },
  { value: 'docker_gc', label: 'Docker garbage collection', hint: 'Removes unreferenced image layers' },
];

function maintenanceConfig(d: Draft): Record<string, unknown> {
  if (d.maintenance === 'blob_compaction') return d.blobStore ? { blob_store: d.blobStore } : {};
  const days = num(d.olderThanDays);
  return {
    ...(days !== undefined ? { older_than_days: days } : {}),
    ...(d.gcRepository ? { repositories: [d.gcRepository] } : {}),
    ...(d.dryRun ? { dry_run: true } : {}),
  };
}

function maintenanceDescription(d: Draft): string {
  if (d.maintenance === 'blob_compaction') return `Blob compaction · ${d.blobStore || 'all blob stores'}`;
  return `Docker garbage collection · ${d.gcRepository || 'all Docker repositories'}`;
}

const num = (s: string) => (s.trim() === '' ? undefined : Number(s));

/** Fields inside "More options": an error in one of them opens the section. */
const MORE_OPTIONS = ['schedule', 'rps', 'concurrency'];

function sourceOf(d: Draft) {
  return {
    url: d.url.trim(),
    ...(d.username.trim() ? { username: d.username.trim() } : {}),
    ...(d.secret ? (d.secretKind === 'token' ? { token: d.secret } : { password: d.secret }) : {}),
  };
}

function filtersOf(d: Draft) {
  const latest = num(d.latest);
  return {
    ...(splitList(d.packages).length ? { packages: splitList(d.packages) } : {}),
    ...(splitList(d.exclude).length ? { exclude: splitList(d.exclude) } : {}),
    ...(d.versions.trim() ? { versions: d.versions.trim() } : {}),
    ...(latest ? { latest } : {}),
    ...(d.prereleases ? { prereleases: true } : {}),
  };
}

function aptOf(d: Draft) {
  return {
    ...(d.distribution.trim() ? { distribution: d.distribution.trim() } : {}),
    ...(splitList(d.components).length ? { components: splitList(d.components) } : {}),
    ...(splitList(d.architectures).length ? { architectures: splitList(d.architectures) } : {}),
  };
}

function tuningOf(d: Draft) {
  return {
    ...(num(d.rps) ? { requests_per_second: num(d.rps) } : {}),
    ...(num(d.concurrency) ? { concurrency: num(d.concurrency) } : {}),
  };
}

/** The task config the form describes. */
function buildConfig(kind: ImportKind, d: Draft, format: string): ImportTaskConfig | PrewarmTaskConfig {
  if (kind === 'import') {
    return {
      repository: d.repository,
      source: sourceOf(d),
      ...filtersOf(d),
      ...(format === 'apt' ? aptOf(d) : {}),
      ...(format === 'raw' && splitList(d.files).length ? { files: splitList(d.files) } : {}),
      ...tuningOf(d),
    };
  }
  const coordinates = splitList(d.coordinates);
  return {
    repository: d.repository,
    ...(coordinates.length ? { coordinates } : {}),
    ...(d.inventory ? { inventory: { source: sourceOf(d), ...filtersOf(d), ...(format === 'apt' ? aptOf(d) : {}) } } : {}),
    ...tuningOf(d),
  };
}

function validate(kind: NewTaskKind, d: Draft): Record<string, string> {
  const e: Record<string, string> = {};
  if (!TASK_NAME_RE.test(d.name)) e.name = 'Lower-case letters, digits and dashes, 2 to 63 characters';
  if (scheduleToCron(d.schedule) === undefined) e.schedule = 'Complete the schedule';
  if (kind === 'maintenance') {
    const days = d.olderThanDays.trim();
    if (d.maintenance === 'docker_gc' && days !== '' && !(Number.isInteger(Number(days)) && Number(days) >= 0)) e.olderThanDays = 'Whole days, 0 or more';
    return e;
  }
  if (!d.repository) e.repository = kind === 'import' ? 'Choose the hosted repository to import into' : 'Choose the proxy or group to fill';
  const needSource = kind === 'import' || d.inventory;
  if (needSource && !/^https?:\/\/[^\s/]+/i.test(d.url.trim().replace(/^sparse\+/, ''))) e.url = 'An http(s) URL of the source registry';
  if (kind === 'prewarm' && !d.inventory && splitList(d.coordinates).length === 0) e.coordinates = 'Paste coordinates, upload a lockfile or list a source';
  for (const [k, v] of [
    ['rps', d.rps],
    ['concurrency', d.concurrency],
    ['latest', d.latest],
  ] as const) {
    if (v.trim() !== '' && !(Number(v) >= 0)) e[k] = 'A positive number';
  }
  return e;
}

export function NewTaskPage() {
  const search = route.useSearch();
  const navigate = route.useNavigate();
  const canWrite = useCanSystem('tasks', 'write');
  const kind: NewTaskKind = search.kind ?? 'import';
  const repos = useRepositories({ limit: 1000 });
  const stores = useBlobStores(kind === 'maintenance');
  const [draft, setDraft] = useState<Draft>(() => ({ ...EMPTY, repository: search.repository ?? '', runNow: kind !== 'maintenance' }));
  const [shown, setShown] = useState(false);
  const [chips, setChips] = useState<LockfileChip[]>([]);
  const [lockError, setLockError] = useState<string | null>(null);
  const create = useCreateTask();
  const parse = useParseLockfile();
  const fileRef = useRef<HTMLInputElement>(null);
  const id = useId();
  const f = (k: string) => `${id}-${k}`;

  const candidates = useMemo(
    () =>
      (repos.data?.items ?? []).filter((r: Repository) =>
        kind === 'maintenance' ? r.format === 'docker' && r.type !== 'group' : kind === 'import' ? r.type === 'hosted' : r.type === 'proxy' || r.type === 'group',
      ),
    [repos.data, kind],
  );
  const repo = candidates.find((r) => r.name === draft.repository);
  const format = repo?.format ?? '';
  const hints = importHints(format);
  const errors = validate(kind, draft);
  const set = (patch: Partial<Draft>) => setDraft((d) => ({ ...d, ...patch }));
  const err = (k: string) => (shown ? errors[k] : undefined);
  /** Props tying a control to its error message. */
  const invalid = (k: string, hintId?: string) => ({
    'aria-invalid': err(k) ? true : undefined,
    'aria-describedby': [err(k) ? f(`${k}-err`) : '', hintId ?? ''].filter(Boolean).join(' ') || undefined,
  });
  const fieldError = (k: string) =>
    err(k) ? (
      <span id={f(`${k}-err`)} className="field__error">
        {err(k)}
      </span>
    ) : null;
  const errorCount = Object.keys(errors).length;
  // An error in a closed "More options" would be invisible: open it.
  const more = useDetailsOpen(shown && MORE_OPTIONS.some((k) => errors[k]));
  const formRef = useRef<HTMLFormElement>(null);
  // Each refused submit moves the focus to the first field with a problem, once the section holding it is open.
  const [attempts, setAttempts] = useState(0);
  useEffect(() => {
    if (!attempts) return;
    const form = formRef.current;
    const first =
      form?.querySelector<HTMLElement>('[aria-invalid="true"]') ??
      form?.querySelector('.field__error')?.closest('.field, fieldset')?.querySelector<HTMLElement>('input, textarea, button, [tabindex]');
    first?.focus();
  }, [attempts]);

  const switchKind = (k: NewTaskKind) => {
    void navigate({ search: { kind: k, repository: undefined }, replace: true });
    // Maintenance deletes data: it does not start on its own unless asked.
    setDraft((d) => ({ ...d, repository: '', runNow: k !== 'maintenance' }));
    setChips([]);
  };

  const onFiles = async (files: FileList | null) => {
    if (!files || !draft.repository) return;
    setLockError(null);
    for (const file of Array.from(files)) {
      try {
        const res = await parse.mutateAsync({ repository: draft.repository, file });
        setDraft((d) => ({ ...d, coordinates: [...new Set([...splitList(d.coordinates), ...res.coordinates])].join('\n') }));
        setChips((c) => [...c.filter((x) => x.name !== file.name), { name: file.name, entries: res.coordinates.length, skipped: res.skipped }]);
      } catch (e) {
        setLockError(`${file.name}: ${errorText(e, { action: 'read the file' })}`);
      }
    }
    if (fileRef.current) fileRef.current.value = '';
  };

  const onSubmit = (e: FormEvent) => {
    e.preventDefault();
    setShown(true);
    if (errorCount) {
      setAttempts((n) => n + 1);
      return;
    }
    const schedule = scheduleToCron(draft.schedule) ?? null;
    create.mutate(
      {
        input:
          kind === 'maintenance'
            ? { name: draft.name, type: draft.maintenance, description: maintenanceDescription(draft), schedule, config: maintenanceConfig(draft) }
            : {
                name: draft.name,
                type: kind === 'import' ? 'repository_import' : 'proxy_prewarm',
                description: kind === 'import' ? `Import into ${draft.repository}` : `Pre-warm ${draft.repository}`,
                schedule,
                config: buildConfig(kind, draft, format) as Record<string, unknown>,
              },
        runNow: draft.runNow,
      },
      {
        onSuccess: ({ task, run }) => void navigate({ to: '/tasks/$taskId', params: { taskId: task.id }, search: run ? { run: run.id } : {} }),
      },
    );
  };

  const coordCount = splitList(draft.coordinates).length;
  const needSource = kind === 'import' || (kind === 'prewarm' && draft.inventory);
  const scheduleField = <ScheduleField value={draft.schedule} onChange={(schedule) => set({ schedule })} error={err('schedule')} />;

  return (
    <div className="screen">
      <PageHeader
        eyebrow={<Link to="/tasks">Scheduled tasks</Link>}
        title="New task"
        description="Import packages from another registry, fill a proxy cache before clients ask for them, or schedule maintenance."
      />
      {!canWrite ? (
        <Panel>
          <p className="meta">Creating tasks needs the tasks write privilege.</p>
        </Panel>
      ) : (
        <form ref={formRef} className="new-task" onSubmit={onSubmit} noValidate aria-label="New task">
          <Panel>
            <Segmented<NewTaskKind>
              className="new-task__kind"
              label="Task type"
              value={kind}
              onChange={switchKind}
              options={[
                { value: 'import', label: 'Import packages' },
                { value: 'prewarm', label: 'Pre-warm a proxy' },
                { value: 'maintenance', label: 'Maintenance' },
              ]}
            />
            <div className="form-grid new-task__grid">
              <div className="field">
                <label className="field__label" htmlFor={f('name')}>
                  Task name
                </label>
                <input
                  id={f('name')}
                  className="input input--mono"
                  value={draft.name}
                  placeholder={kind === 'import' ? 'import-npm' : kind === 'prewarm' ? 'prewarm-npm' : draft.maintenance === 'docker_gc' ? 'docker-gc-weekly' : 'compaction-weekly'}
                  {...invalid('name')}
                  onChange={(e) => set({ name: e.target.value.trim() })}
                  autoComplete="off"
                  spellCheck={false}
                />
                {fieldError('name')}
              </div>
              {kind === 'maintenance' && (
                <>
                  <div className="field">
                    <label className="field__label" htmlFor={f('mtype')}>
                      Maintenance
                    </label>
                    <Select<MaintenanceType> id={f('mtype')} value={draft.maintenance} options={MAINTENANCE} onChange={(v) => set({ maintenance: v })} />
                  </div>
                  {draft.maintenance === 'blob_compaction' ? (
                    <div className="field">
                      <label className="field__label" htmlFor={f('store')}>
                        Blob store
                      </label>
                      <Select
                        id={f('store')}
                        value={draft.blobStore}
                        options={[{ value: '', label: 'All blob stores' }, ...(stores.data?.items ?? []).map((b) => ({ value: b.name, label: b.name, hint: b.location }))]}
                        onChange={(v) => set({ blobStore: v })}
                      />
                    </div>
                  ) : (
                    <>
                      <div className="field">
                        <label className="field__label" htmlFor={f('gcrepo')}>
                          Docker repository
                        </label>
                        <Select
                          id={f('gcrepo')}
                          value={draft.gcRepository}
                          searchable
                          options={[{ value: '', label: 'All Docker repositories' }, ...candidates.map((r) => ({ value: r.name, label: r.name, hint: r.type }))]}
                          onChange={(v) => set({ gcRepository: v })}
                        />
                      </div>
                      <div className="field">
                        <label className="field__label" htmlFor={f('days')}>
                          Unreferenced for <span className="field__optional">days</span>
                        </label>
                        <input
                          id={f('days')}
                          className="input"
                          type="number"
                          min={0}
                          inputMode="numeric"
                          placeholder="7"
                          value={draft.olderThanDays}
                          {...invalid('olderThanDays')}
                          onChange={(e) => set({ olderThanDays: e.target.value })}
                        />
                        {fieldError('olderThanDays')}
                      </div>
                      <label className="checkbox">
                        <input type="checkbox" checked={draft.dryRun} onChange={(e) => set({ dryRun: e.target.checked })} /> Dry run: only report what
                        would be removed
                      </label>
                    </>
                  )}
                  <div className="field field--wide">{scheduleField}</div>
                </>
              )}

              {kind !== 'maintenance' && (
                <div className="field">
                  <label className="field__label" htmlFor={f('repo')}>
                    {kind === 'import' ? 'Hosted repository' : 'Proxy or group repository'}
                  </label>
                  <Select
                    id={f('repo')}
                    value={draft.repository}
                    searchable
                    placeholder={repos.isPending ? 'Loading…' : repos.isError ? 'Repositories could not be loaded' : candidates.length ? 'Choose a repository' : 'No repositories of this kind'}
                    options={candidates.map((r) => ({ value: r.name, label: r.name, hint: `${r.format} · ${r.type}` }))}
                    {...invalid('repository')}
                    onChange={(v) => {
                      set({ repository: v });
                      setChips([]);
                    }}
                  />
                  {repos.isError ? <PickerLoadError what="repositories" onRetry={() => void repos.refetch()} /> : fieldError('repository')}
                </div>
              )}

              {kind === 'prewarm' && (
                <div className="field field--wide">
                  <label className="field__label" htmlFor={f('coords')}>
                    Coordinates <span className="field__optional">{coordCount ? `· ${coordCount}` : ''}</span>
                  </label>
                  <textarea
                    id={f('coords')}
                    className="input input--mono textarea new-task__coords"
                    value={draft.coordinates}
                    placeholder={hints.coordinates}
                    {...invalid('coordinates')}
                    onChange={(e) => set({ coordinates: e.target.value })}
                    spellCheck={false}
                  />
                  <div className="new-task__files">
                    <input
                      ref={fileRef}
                      id={f('lock')}
                      type="file"
                      multiple
                      className="visually-hidden"
                      onChange={(e) => void onFiles(e.target.files)}
                      disabled={!draft.repository || parse.isPending}
                    />
                    <label htmlFor={f('lock')} className={`button button--secondary button--sm${!draft.repository ? ' is-disabled' : ''}`} aria-disabled={!draft.repository || undefined}>
                      {parse.isPending ? 'Reading…' : 'Add from lockfile'}
                    </label>
                    <span className="field__hint">{draft.repository ? hints.lockfiles : 'Choose the repository first'}</span>
                    {chips.map((c) => (
                      <span key={c.name} className="new-task__chip">
                        {c.name} · {c.entries} added{c.skipped ? `, ${c.skipped} skipped` : ''}
                      </span>
                    ))}
                  </div>
                  {lockError && (
                    <span className="field__error" role="alert">
                      {lockError}
                    </span>
                  )}
                  {fieldError('coordinates')}
                  <label className="checkbox">
                    <input type="checkbox" checked={draft.inventory} onChange={(e) => set({ inventory: e.target.checked })} /> Also fetch
                    everything a source registry holds
                  </label>
                </div>
              )}

              {needSource && (
                <>
                  <div className="field field--wide">
                    <label className="field__label" htmlFor={f('url')}>
                      Source URL
                    </label>
                    <input
                      id={f('url')}
                      className="input input--mono"
                      value={draft.url}
                      placeholder={hints.url}
                      aria-invalid={err('url') ? true : undefined}
                      aria-describedby={f('url-note')}
                      onChange={(e) => set({ url: e.target.value })}
                      autoComplete="off"
                      spellCheck={false}
                    />
                    <span id={f('url-note')} className={err('url') ? 'field__error' : 'field__hint'}>
                      {err('url') ?? hints.source}
                    </span>
                  </div>
                  <label className="field" htmlFor={f('user')}>
                    <span>
                      Username <span className="field__optional">optional</span>
                    </span>
                    <input id={f('user')} className="input" value={draft.username} onChange={(e) => set({ username: e.target.value })} autoComplete="off" />
                  </label>
                  <div className="field">
                    <span className="new-task__secret-head">
                      <label className="field__label" htmlFor={f('secret')}>
                        {draft.secretKind === 'token' ? 'Token' : 'Password'} <span className="field__optional">optional · stored encrypted</span>
                      </label>
                      <button
                        type="button"
                        className="link-button"
                        onClick={() => set({ secretKind: draft.secretKind === 'token' ? 'password' : 'token' })}
                      >
                        Use a {draft.secretKind === 'token' ? 'password' : 'token'}
                      </button>
                    </span>
                    <input
                      id={f('secret')}
                      className="input"
                      type="password"
                      value={draft.secret}
                      onChange={(e) => set({ secret: e.target.value })}
                      autoComplete="new-password"
                    />
                  </div>
                  <label className="field" htmlFor={f('pkgs')}>
                    <span>
                      Packages <span className="field__optional">{hints.packagesOptional ? 'optional · all when empty' : 'exact names'}</span>
                    </span>
                    <textarea
                      id={f('pkgs')}
                      className="input input--mono textarea new-task__short"
                      value={draft.packages}
                      placeholder={hints.packages}
                      onChange={(e) => set({ packages: e.target.value })}
                      spellCheck={false}
                    />
                  </label>
                  <div className="new-task__versions">
                    <label className="field" htmlFor={f('versions')}>
                      <span>
                        Versions <span className="field__optional">optional</span>
                      </span>
                      <input
                        id={f('versions')}
                        className="input input--mono"
                        value={draft.versions}
                        placeholder={hints.versions}
                        onChange={(e) => set({ versions: e.target.value })}
                        spellCheck={false}
                      />
                    </label>
                    <div className="field">
                      <label className="field__label" htmlFor={f('latest')}>
                        Newest <span className="field__optional">per package</span>
                      </label>
                      <input
                        id={f('latest')}
                        className="input"
                        type="number"
                        min={0}
                        inputMode="numeric"
                        value={draft.latest}
                        placeholder="All"
                        {...invalid('latest')}
                        onChange={(e) => set({ latest: e.target.value })}
                      />
                      {fieldError('latest')}
                    </div>
                    <label className="checkbox">
                      <input type="checkbox" checked={draft.prereleases} onChange={(e) => set({ prereleases: e.target.checked })} /> Pre-releases
                    </label>
                  </div>
                </>
              )}
            </div>

            {kind !== 'maintenance' && (
              <details className="new-task__more" open={more.open} onToggle={more.onToggle}>
                <summary>More options</summary>
                <div className="new-task__schedule">{scheduleField}</div>
                <div className="form-grid form-grid--3">
                  {needSource && (
                    <label className="field" htmlFor={f('exclude')}>
                      Exclude
                      <input
                        id={f('exclude')}
                        className="input input--mono"
                        value={draft.exclude}
                        placeholder="internal-*"
                        onChange={(e) => set({ exclude: e.target.value })}
                        spellCheck={false}
                      />
                    </label>
                  )}
                  {needSource && format === 'apt' && (
                    <>
                      <label className="field" htmlFor={f('dist')}>
                        Suite
                        <input id={f('dist')} className="input input--mono" value={draft.distribution} placeholder="stable" onChange={(e) => set({ distribution: e.target.value })} />
                      </label>
                      <label className="field" htmlFor={f('comps')}>
                        Components
                        <input id={f('comps')} className="input input--mono" value={draft.components} placeholder="main" onChange={(e) => set({ components: e.target.value })} />
                      </label>
                      <label className="field" htmlFor={f('archs')}>
                        Architectures
                        <input id={f('archs')} className="input input--mono" value={draft.architectures} placeholder="amd64" onChange={(e) => set({ architectures: e.target.value })} />
                      </label>
                    </>
                  )}
                  {kind === 'import' && format === 'raw' && (
                    <label className="field field--wide" htmlFor={f('files')}>
                      Files <span className="field__hint">paths relative to the source URL; end a directory with /</span>
                      <textarea
                        id={f('files')}
                        className="input input--mono textarea new-task__short"
                        value={draft.files}
                        placeholder={'tools/\ndocs/guide.pdf'}
                        onChange={(e) => set({ files: e.target.value })}
                      />
                    </label>
                  )}
                  <div className="field">
                    <label className="field__label" htmlFor={f('rps')}>
                      Requests per second
                    </label>
                    <input
                      id={f('rps')}
                      className="input"
                      type="number"
                      min={0}
                      value={draft.rps}
                      placeholder="10"
                      {...invalid('rps')}
                      onChange={(e) => set({ rps: e.target.value })}
                    />
                    {fieldError('rps')}
                  </div>
                  <div className="field">
                    <label className="field__label" htmlFor={f('conc')}>
                      At once
                    </label>
                    <input
                      id={f('conc')}
                      className="input"
                      type="number"
                      min={0}
                      max={16}
                      value={draft.concurrency}
                      placeholder="4"
                      {...invalid('concurrency')}
                      onChange={(e) => set({ concurrency: e.target.value })}
                    />
                    {fieldError('concurrency')}
                  </div>
                </div>
              </details>
            )}

            {shown && errorCount > 0 && (
              <p className="form-error" role="alert">
                {errorCount === 1 ? 'One field needs attention; it is marked above.' : `${errorCount} fields need attention; they are marked above.`}
              </p>
            )}
            {create.isError && (
              <p className="form-error" role="alert">
                {errorText(create.error, { action: 'create the task' })}
              </p>
            )}
            <div className="new-task__actions">
              <label className="checkbox">
                <input type="checkbox" checked={draft.runNow} onChange={(e) => set({ runNow: e.target.checked })} /> Run now
              </label>
              <Link to="/tasks" className="button button--secondary">
                Cancel
              </Link>
              <button type="submit" className="button button--primary" disabled={create.isPending}>
                {create.isPending ? 'Creating…' : draft.runNow ? 'Create and run' : 'Create task'}
              </button>
            </div>
          </Panel>
        </form>
      )}
    </div>
  );
}
