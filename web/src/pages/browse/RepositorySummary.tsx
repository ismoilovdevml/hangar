import { Link } from '@tanstack/react-router';
import { ApiRequestError } from '../../api/client';
import { useAllRepositories, useRepository } from '../../api/catalog';
import { CodeBlock } from '../../components/CodeBlock';
import { CopyButton } from '../../components/CopyButton';
import { ErrorNote } from '../../components/ErrorNote';
import { IconSettings } from '../../components/opsIcons';
import { Panel } from '../../components/Panel';
import { Pill, StatusDot } from '../../components/Pill';
import { FORMAT_META, formatBytes } from '../../lib/format';
import { repoClientConfig } from '../../lib/repoSettings';
import { isFormat, STATUS_META, TYPE_META } from '../../lib/repositories';
import { FormatIcon } from './formatIcon';

/** Right column when a repository is chosen but no package yet. */
export function RepositorySummary({ repo }: { repo: string }) {
  const q = useRepository(repo);
  if (q.isError) {
    const notFound = q.error instanceof ApiRequestError && q.error.status === 404;
    return (
      <Panel className="browse-empty" role="status">
        {notFound ? <p>Repository {repo} does not exist, or you cannot see it.</p> : <ErrorNote what="the repository" onRetry={() => void q.refetch()} />}
      </Panel>
    );
  }
  const r = q.data;
  if (!r) {
    return (
      <Panel aria-busy="true" className="browse-empty">
        <p>Loading {repo}…</p>
      </Panel>
    );
  }
  const status = STATUS_META[r.status];
  return (
    <>
      <Panel className="pkg-head">
        <div className="pkg-head__row">
          <FormatIcon format={r.format} size={48} />
          <div className="pkg-head__text">
            <h1 className="pkg-head__title">{r.name}</h1>
            <p className="pkg-head__meta">
              {r.description || `${isFormat(r.format) ? FORMAT_META[r.format].label : r.format} repository`}
            </p>
          </div>
          <div className="pkg-head__actions">
            <Link to="/repositories/$name/settings" params={{ name: r.name }} className="button button--secondary">
              <IconSettings size={16} /> Settings
            </Link>
          </div>
        </div>
        <dl className="facts">
          <div>
            <dt>Type</dt>
            <dd>
              <Pill tone={TYPE_META[r.type].tone}>{TYPE_META[r.type].label}</Pill>
            </dd>
          </div>
          <div>
            <dt>Format</dt>
            <dd>{isFormat(r.format) ? FORMAT_META[r.format].label : r.format}</dd>
          </div>
          <div>
            <dt>Status</dt>
            <dd>
              <StatusDot tone={status.tone} label={status.label} />
            </dd>
          </div>
          {r.type !== 'group' && (
            <>
              <div>
                <dt>Stored</dt>
                <dd>{formatBytes(r.size_bytes)}</dd>
              </div>
              <div>
                <dt>Versions</dt>
                <dd>{r.component_count}</dd>
              </div>
              {r.blob_store && (
                <div>
                  <dt>Blob store</dt>
                  <dd>{r.blob_store}</dd>
                </div>
              )}
            </>
          )}
          {r.type === 'proxy' && r.config.proxy?.remote_url && (
            <div className="facts__wide">
              <dt>Upstream</dt>
              <dd className="facts__mono facts__copy">
                <span>{r.config.proxy.remote_url}</span>
                <CopyButton text={r.config.proxy.remote_url} label={`Copy upstream URL of ${r.name}`} />
              </dd>
            </div>
          )}
          <div className="facts__wide">
            <dt>URL</dt>
            <dd className="facts__mono facts__copy">
              <span>{r.url}</span>
              <CopyButton text={r.url} label={`Copy URL of ${r.name}`} />
            </dd>
          </div>
        </dl>
      </Panel>
      {r.type === 'group' && <GroupMembers members={r.config.group?.members ?? []} writeMember={r.config.group?.write_member} />}
      <Panel title="Use this repository" titleId="use-repo-title">
        <CodeBlock label="client configuration" lines={repoClientConfig(r)} />
        <p className="meta">
          {r.type === 'group'
            ? 'Expand the group on the left to browse the content of all its members.'
            : r.component_count === 0
              ? 'Nothing has been published here yet.'
              : 'Expand the repository on the left to browse its packages and files.'}
        </p>
      </Panel>
    </>
  );
}

/** The members a group resolves from, in order, with what each one is. */
function GroupMembers({ members, writeMember }: { members: string[]; writeMember?: string }) {
  const all = useAllRepositories();
  const byName = new Map((all.data ?? []).map((r) => [r.name, r]));
  return (
    <Panel title="Members" titleId="group-members-title">
      <p className="meta">Requests are answered by the first member, in this order, that has the file.</p>
      {members.length === 0 ? (
        <p className="meta">This group has no members.</p>
      ) : (
        <ol className="member-list">
          {members.map((m) => {
            const r = byName.get(m);
            return (
              <li key={m} className="member-list__item">
                {r && <FormatIcon format={r.format} size={24} />}
                <Link to="/browse" search={{ repo: m }} className="link-strong">
                  {m}
                </Link>
                {r && <Pill tone={TYPE_META[r.type].tone}>{TYPE_META[r.type].label}</Pill>}
                {m === writeMember && <Pill tone="info">Receives publishes</Pill>}
                {r && r.type !== 'group' && <span className="meta">{formatBytes(r.size_bytes)}</span>}
              </li>
            );
          })}
        </ol>
      )}
    </Panel>
  );
}
