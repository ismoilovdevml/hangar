import { Link } from '@tanstack/react-router';
import { ApiRequestError } from '../../api/client';
import { useRepository } from '../../api/catalog';
import { CodeBlock } from '../../components/CodeBlock';
import { CopyButton } from '../../components/CopyButton';
import { ErrorNote } from '../../components/ErrorNote';
import { FormatBadge } from '../../components/FormatBadge';
import { IconSettings } from '../../components/opsIcons';
import { Panel } from '../../components/Panel';
import { Pill, StatusDot } from '../../components/Pill';
import { FORMAT_META, formatBytes } from '../../lib/format';
import { repoClientConfig } from '../../lib/repoSettings';
import { isFormat, STATUS_META, TYPE_META } from '../../lib/repositories';

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
          {isFormat(r.format) && <FormatBadge format={r.format} size={48} />}
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
            </>
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
      <Panel title="Use this repository" titleId="use-repo-title">
        <CodeBlock label="client configuration" lines={repoClientConfig(r)} />
        <p className="meta">
          {r.type === 'group'
            ? 'Pick a member on the left to see its packages.'
            : r.component_count === 0
              ? 'Nothing has been published here yet.'
              : 'Pick a package on the left to see its versions and files.'}
        </p>
      </Panel>
    </>
  );
}
