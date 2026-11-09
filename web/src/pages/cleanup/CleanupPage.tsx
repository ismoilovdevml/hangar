import { useState } from 'react';
import { getRouteApi, Link } from '@tanstack/react-router';
import { isMissingEndpoint, useAllCleanupPolicies, useCanSystem } from '../../api/ops';
import { ErrorNote } from '../../components/ErrorNote';
import { IconPlus } from '../../components/icons';
import { PageHeader } from '../../components/PageHeader';
import { Panel } from '../../components/Panel';
import { summarizeCriteria } from '../../lib/cleanup';
import { PolicyEditor } from './PolicyEditor';

const route = getRouteApi('/app/cleanup');

export interface CleanupSearch {
  policy?: string;
  create?: boolean;
}

export function CleanupPage() {
  const { policy: selectedName, create } = route.useSearch();
  const navigate = route.useNavigate();
  const policies = useAllCleanupPolicies();
  const canWrite = useCanSystem('cleanup', 'write');
  const canDelete = useCanSystem('cleanup', 'delete');
  const [notice, setNotice] = useState<string | null>(null);

  const list = policies.data ?? [];
  const selected = create ? undefined : (list.find((p) => p.name === selectedName) ?? (selectedName ? undefined : list[0]));
  const missing = !create && !!selectedName && policies.isSuccess && !selected;
  const showEditor = (create && canWrite) || !!selected;

  return (
    <div className="screen">
      <PageHeader
        title="Cleanup policies"
        description="Rules that quietly remove what nobody uses. Always preview before they run."
        actions={
          canWrite && (
            <Link to="/cleanup" search={{ create: true }} className="button button--primary">
              <IconPlus size={16} /> New policy
            </Link>
          )
        }
      />

      <p className="visually-hidden" role="status">
        {notice ?? ''}
      </p>
      {notice && (
        <div className="notice" aria-hidden="true">
          {notice}
          <button type="button" className="button button--secondary button--sm" onClick={() => setNotice(null)}>
            Dismiss
          </button>
        </div>
      )}

      <div className="cleanup-layout">
        <Panel as="nav" className="policy-list" aria-label="Cleanup policies">
          {policies.isError && isMissingEndpoint(policies.error) ? (
            <p className="meta">This server does not support cleanup policies yet. Upgrade Hangar to manage them here.</p>
          ) : policies.isError ? (
            <ErrorNote what="cleanup policies" error={policies.error} privilege="system:cleanup:read" onRetry={() => void policies.refetch()} />
          ) : policies.isPending ? (
            <p className="meta">Loading policies…</p>
          ) : (
            <ul className="policy-list__items">
              {list.map((p) => {
                const active = !create && selected?.id === p.id;
                return (
                  <li key={p.id}>
                    <Link
                      to="/cleanup"
                      search={{ policy: p.name }}
                      className={active ? 'policy-item is-active' : 'policy-item'}
                      aria-current={active ? 'page' : undefined}
                      onClick={() => setNotice(null)}
                    >
                      <span className="policy-item__name">{p.name}</span>
                      {p.description && <span className="policy-item__desc">{p.description}</span>}
                      <span className="policy-item__meta">
                        {p.repositories.length} {p.repositories.length === 1 ? 'repository' : 'repositories'} ·{' '}
                        {p.enabled === false ? 'Paused' : p.schedule_text}
                      </span>
                      <span className="policy-item__meta">{summarizeCriteria(p.criteria)}</span>
                    </Link>
                  </li>
                );
              })}
              {list.length === 0 && <li className="meta">No policies yet.</li>}
            </ul>
          )}
          {canWrite && (
            <Link to="/cleanup" search={{ create: true }} className={create ? 'policy-new is-active' : 'policy-new'} aria-current={create ? 'page' : undefined}>
              New policy
            </Link>
          )}
        </Panel>

        <div className="cleanup-main">
          {missing && (
            <Panel>
              <p>
                There is no policy named <strong>{selectedName}</strong>. It may have been deleted. <Link to="/cleanup">Show all policies</Link>.
              </p>
            </Panel>
          )}
          {create && !canWrite && (
            <Panel>
              <p>Creating cleanup policies needs the cleanup write privilege.</p>
            </Panel>
          )}
          {showEditor && (
            <PolicyEditor
              key={selected?.id ?? 'new'}
              policy={selected}
              canWrite={canWrite}
              canDelete={canDelete}
              onSaved={(name, message) => {
                setNotice(message);
                void navigate({ search: { policy: name } });
              }}
              onDeleted={(message) => {
                setNotice(message);
                void navigate({ search: {} });
              }}
            />
          )}
          {!showEditor && !missing && !create && policies.isSuccess && list.length === 0 && (
            <Panel title="No cleanup policies yet" titleId="cleanup-empty-title">
              <p className="meta">
                A policy deletes versions nobody uses — old, never downloaded, or pre-release builds — while always keeping the newest
                ones and the tags you name. Every policy is previewed before it is saved.
              </p>
              {canWrite && (
                <p>
                  <Link to="/cleanup" search={{ create: true }}>
                    Create the first policy
                  </Link>
                </p>
              )}
            </Panel>
          )}
        </div>
      </div>
    </div>
  );
}
