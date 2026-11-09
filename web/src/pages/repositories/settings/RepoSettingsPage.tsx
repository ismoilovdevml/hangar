import { useMemo, useState, type FormEvent } from 'react';
import { getRouteApi, Link, useNavigate } from '@tanstack/react-router';
import { ApiRequestError } from '../../../api/client';
import { useDeleteRepository, useRepository, useUpdateRepository } from '../../../api/catalog';
import { useRepositoriesByFormat } from '../../../api/hooks';
import { usePermissions } from '../../../api/permissions';
import type { Repository } from '../../../api/types';
import { CodeBlock } from '../../../components/CodeBlock';
import { ConfirmDialog } from '../../../components/ConfirmDialog';
import { CopyButton } from '../../../components/CopyButton';
import { ErrorNote } from '../../../components/ErrorNote';
import { FormatBadge } from '../../../components/FormatBadge';
import { IconPackage } from '../../../components/icons';
import { ClientSetup } from '../../../components/ClientSetup';
import { FormatName } from '../../../components/FormatIcon';
import { PageHeader } from '../../../components/PageHeader';
import { Panel } from '../../../components/Panel';
import { Pill, StatusDot } from '../../../components/Pill';
import { FORMAT_META, formatBytes } from '../../../lib/format';
import { isDirty, repoClientTarget, repoToForm, storedInput, toRepositoryUpdate } from '../../../lib/repoSettings';
import {
  isFormat,
  repositoryYaml,
  SIGNED_FORMATS,
  secretStored,
  STATUS_META,
  TYPE_META,
  validateRepoForm,
  type RepoForm,
  type RepoFormErrors,
  type RepoFormField,
} from '../../../lib/repositories';
import { RepoSettingsFields } from '../RepoFormFields';
import { focusFirstError } from '../repoFormHelpers';
import { RoutingPanel } from './RoutingPanel';
import { SigningKeyPanel } from './SigningKeyPanel';
import '../../../styles/catalog.css';
import './settings.css';

const route = getRouteApi('/app/repositories/$name/settings');

function apiMessage(err: unknown, action: string): string {
  if (err instanceof ApiRequestError) {
    if (err.status === 403) return `You do not have permission to ${action} this repository.`;
    return err.body?.message ?? `The server could not ${action} the repository.`;
  }
  return 'Cannot reach the server. Check your connection and try again.';
}

export function RepoSettingsPage() {
  const { name } = route.useParams();
  const repo = useRepository(name);

  if (repo.isError) {
    const notFound = repo.error instanceof ApiRequestError && repo.error.status === 404;
    return (
      <div className="screen">
        <PageHeader title={name} description={notFound ? 'This repository does not exist, or you cannot see it.' : undefined} />
        {notFound ? (
          <p>
            <Link to="/repositories">Back to repositories</Link>
          </p>
        ) : (
          <ErrorNote what="the repository" onRetry={() => void repo.refetch()} />
        )}
      </div>
    );
  }
  if (!repo.data) {
    return (
      <div className="screen" aria-busy="true">
        <PageHeader title={name} description="Loading settings…" />
      </div>
    );
  }
  return <SettingsForm key={repo.data.id} repo={repo.data} />;
}

function SettingsForm({ repo }: { repo: Repository }) {
  const navigate = useNavigate();
  const perms = usePermissions();
  const canAdmin = perms.canAdminRepo(repo.name);
  // Import (hosted) and pre-warm (proxy, group) tasks need tasks and repository write.
  const canImport = perms.can('write', 'system', 'tasks') && perms.can('write', 'repo', repo.name);
  const [form, setForm] = useState<RepoForm>(() => repoToForm(repo));
  const [blurred, setBlurred] = useState<Partial<Record<RepoFormField, boolean>>>({});
  const [submitted, setSubmitted] = useState(false);
  const [saved, setSaved] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);

  const update = useUpdateRepository(repo.name);
  const toggle = useUpdateRepository(repo.name);
  const remove = useDeleteRepository(repo.name);
  const members = useRepositoriesByFormat(repo.type === 'group' ? repo.format : undefined);

  const set = <K extends keyof RepoForm>(key: K, value: RepoForm[K]) => {
    if (update.isError) update.reset();
    setSaved(false);
    setForm((f) => ({ ...f, [key]: value }));
  };
  const blur = (f: RepoFormField) => () => setBlurred((b) => ({ ...b, [f]: true }));

  const storedAuth = repo.config.proxy?.auth;
  const storedRemoteUrl = repo.config.proxy?.remote_url;
  const memberTypes = useMemo(() => new Map((members.data ?? []).map((r) => [r.name, r.type] as const)), [members.data]);
  const errors = validateRepoForm(form, { memberTypes, storedAuth, storedRemoteUrl });
  const visible: RepoFormErrors = Object.fromEntries(
    Object.entries(errors).filter(([k]) => submitted || blurred[k as RepoFormField]),
  );
  const dirty = isDirty(form, repo);
  const body = toRepositoryUpdate(form, repo);
  const yaml = repositoryYaml(body);
  const online = repo.online ?? true;
  const status = STATUS_META[repo.status];
  const formatLabel = isFormat(repo.format) ? FORMAT_META[repo.format].label : repo.format;

  const onSubmit = (e: FormEvent) => {
    e.preventDefault();
    setSubmitted(true);
    if (focusFirstError(errors)) return;
    update.mutate(body, {
      onSuccess: (next) => {
        setForm(repoToForm(next));
        setSubmitted(false);
        setBlurred({});
        setSaved(true);
      },
    });
  };

  const readonly = (label: string, value: string, hint?: string) => (
    <div className="field">
      <span className="field__label">{label}</span>
      <span className="readonly-value">{value}</span>
      {hint && <span className="field__hint">{hint}</span>}
    </div>
  );

  const identity = (
    <>
      {readonly('Name', repo.name, 'Cannot be changed')}
      {repo.type !== 'group' && readonly('Blob store', repo.blob_store ?? '—', 'Cannot be changed after creation')}
    </>
  );

  return (
    <div className="screen">
      <PageHeader
        eyebrow={
          <Link to="/repositories" className="link-strong">
            Repositories
          </Link>
        }
        title={
          <span className="repo-settings__title">
            {isFormat(repo.format) && <FormatBadge format={repo.format} size={40} />}
            {repo.name}
          </span>
        }
        description={repo.description || `${formatLabel} · ${TYPE_META[repo.type].label.toLowerCase()} repository`}
        actions={
          <>
            <Link to="/browse" search={{ repo: repo.name }} className="button button--secondary">
              <IconPackage size={16} /> Browse packages
            </Link>
            {canImport && (
              <Link
                to="/tasks/new"
                search={{ kind: repo.type === 'hosted' ? 'import' : 'prewarm', repository: repo.name }}
                className="button button--secondary"
              >
                {repo.type === 'hosted' ? 'Import' : 'Pre-warm'}
              </Link>
            )}
            <button
              type="button"
              className="button button--secondary"
              aria-pressed={!online}
              disabled={!canAdmin || toggle.isPending}
              title={canAdmin ? undefined : 'Needs admin rights on this repository'}
              onClick={() => toggle.mutate(storedInput(repo, !online))}
            >
              {toggle.isPending ? 'Saving…' : online ? 'Take offline' : 'Bring online'}
            </button>
          </>
        }
      />
      {toggle.isError && (
        <p className="form-error" role="alert">
          {apiMessage(toggle.error, 'change')}
        </p>
      )}

      <form className="repo-create" onSubmit={onSubmit} noValidate aria-label={`Settings of ${repo.name}`}>
        <div className="repo-create__steps">
          <Panel title="Settings" titleId="settings-title">
            {!canAdmin && (
              <p className="note" role="note">
                You can view these settings. Changing them needs admin rights on this repository.
              </p>
            )}
            <RepoSettingsFields
              form={form}
              selfName={repo.name}
              set={set}
              setForm={(v) => {
                setSaved(false);
                setForm(v);
              }}
              errors={visible}
              blur={blur}
              identity={identity}
              secretKept={secretStored(form, storedAuth, storedRemoteUrl)}
              disabled={!canAdmin}
            />
          </Panel>

          {isFormat(repo.format) && SIGNED_FORMATS.includes(repo.format) && repo.type !== 'proxy' && <SigningKeyPanel repo={repo} />}

          {repo.type !== 'hosted' && <RoutingPanel repo={repo} memberTypes={memberTypes} />}

          <Panel title="Delete repository" titleId="danger-title" className="danger-zone">
            <p className="danger-zone__text">
              Removes the repository and its {repo.component_count === 1 ? 'one package version' : `${repo.component_count} package versions`}.
              Their files are released for compaction. Groups that list it must drop it first.
            </p>
            <div>
              <button
                type="button"
                className="button button--danger-outline"
                disabled={!canAdmin}
                title={canAdmin ? undefined : 'Needs admin rights on this repository'}
                onClick={() => {
                  remove.reset();
                  setConfirmDelete(true);
                }}
              >
                Delete {repo.name}
              </button>
            </div>
          </Panel>
        </div>

        <aside className="repo-create__summary" aria-labelledby="summary-title">
          <Panel title="Summary" titleId="summary-title" as="div" className="summary">
            <dl className="summary__list">
              <dt>Format</dt>
              <dd>
                <FormatName format={repo.format} />
              </dd>
              <dt>Type</dt>
              <dd>
                <Pill tone={TYPE_META[repo.type].tone}>{TYPE_META[repo.type].label}</Pill>
              </dd>
              <dt>Status</dt>
              <dd>
                <StatusDot tone={status.tone} label={status.label} />
              </dd>
              {repo.type !== 'group' && (
                <>
                  <dt>Stored</dt>
                  <dd>
                    {formatBytes(repo.size_bytes)} · {repo.component_count} {repo.component_count === 1 ? 'version' : 'versions'}
                  </dd>
                </>
              )}
              <dt>URL</dt>
              <dd className="summary__url repo-settings__url">
                <span>{repo.url}</span>
                <CopyButton text={repo.url} label={`Copy URL of ${repo.name}`} />
              </dd>
            </dl>
            <div className="summary__code repo-settings__client">
              <p className="summary__label">Connect a client</p>
              <ClientSetup targets={[repoClientTarget(repo)]} />
            </div>
            <div className="summary__code">
              <p className="summary__label">Same thing as code</p>
              <CodeBlock label="repository configuration as YAML" lines={yaml.trimEnd().split('\n').map((text) => ({ text, comment: text.trimStart().startsWith('#') }))} />
            </div>
            {submitted && Object.keys(errors).length > 0 && (
              <p className="form-error" role="alert">
                {Object.keys(errors).length === 1 ? 'One setting needs attention.' : `${Object.keys(errors).length} settings need attention.`}
              </p>
            )}
            {update.isError && (
              <p className="form-error" role="alert">
                {apiMessage(update.error, 'save')}
              </p>
            )}
            <p className="visually-hidden" aria-live="polite">
              {saved ? 'Settings saved' : ''}
            </p>
            {canAdmin && (
              <div className="repo-settings__save">
                <button type="submit" className="button button--primary button--block" disabled={update.isPending || !dirty}>
                  {update.isPending ? 'Saving…' : saved && !dirty ? 'Saved' : 'Save changes'}
                </button>
                {dirty && (
                  <button
                    type="button"
                    className="button button--secondary button--block"
                    onClick={() => {
                      update.reset();
                      setForm(repoToForm(repo));
                      setSubmitted(false);
                      setBlurred({});
                    }}
                  >
                    Discard changes
                  </button>
                )}
              </div>
            )}
          </Panel>
        </aside>
      </form>

      {confirmDelete && (
        <ConfirmDialog
          title={`Delete ${repo.name}?`}
          confirmLabel="Delete repository"
          typeToConfirm={repo.name}
          tone="danger"
          busy={remove.isPending}
          error={remove.isError ? apiMessage(remove.error, 'delete') : null}
          onCancel={() => setConfirmDelete(false)}
          onConfirm={() =>
            remove.mutate(undefined, {
              onSuccess: () => void navigate({ to: '/repositories' }),
            })
          }
        >
          <p>
            Clients using <code className="inline-code">{repo.url}</code> will stop working. This cannot be undone.
          </p>
        </ConfirmDialog>
      )}
    </div>
  );
}
