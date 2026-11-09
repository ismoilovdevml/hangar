import { Link } from '@tanstack/react-router';
import { isMissingEndpoint, useClusterStatus } from '../../api/ops';
import type { ClusterNode } from '../../api/opsTypes';
import { ErrorNote } from '../../components/ErrorNote';
import { NotAvailable } from '../../components/NotAvailable';
import { PageHeader } from '../../components/PageHeader';
import { Panel } from '../../components/Panel';
import { Pill, StatusDot } from '../../components/Pill';
import { DOCS } from '../../lib/docs';
import { relativeTime, versionText } from '../../lib/format';
import { formatUptime } from '../../lib/system';
import { formatUtc } from '../../lib/tasks';
import { useNow } from '../../lib/useNow';
import './cluster.css';

const ROLE_LABEL: Record<ClusterNode['role'], string> = { leader: 'Leader', follower: 'Follower', edge: 'Edge' };

function NodeCard({ n, now }: { n: ClusterNode; now: Date }) {
  return (
    <li className="node-card glass">
      <div className="node-card__head">
        <span className="node-card__name">{n.name}</span>
        <Pill tone={n.role === 'leader' ? 'info' : 'neutral'}>{ROLE_LABEL[n.role] ?? n.role}</Pill>
      </div>
      <StatusDot tone={n.healthy ? 'ok' : 'bad'} label={n.healthy ? 'Healthy' : 'Not responding'} />
      <dl className="node-card__facts">
        <div>
          <dt>Address</dt>
          <dd className="mono">{n.address || <NotAvailable />}</dd>
        </div>
        <div>
          <dt>Version</dt>
          <dd>{n.version ? versionText(n.version) : <NotAvailable />}</dd>
        </div>
        {n.zone && (
          <div>
            <dt>Zone</dt>
            <dd>{n.zone}</dd>
          </div>
        )}
        <div>
          <dt>Uptime</dt>
          <dd>{n.uptime_seconds !== undefined ? formatUptime(n.uptime_seconds) : <NotAvailable />}</dd>
        </div>
        <div>
          <dt>Last heartbeat</dt>
          <dd>
            <time dateTime={n.last_heartbeat} title={formatUtc(n.last_heartbeat, { seconds: true })}>
              {relativeTime(n.last_heartbeat, now)}
            </time>
          </dd>
        </div>
      </dl>
    </li>
  );
}

/**
 * The nodes serving this registry, from GET /nodes, and an honest word on
 * replication, which the server does not do yet.
 */
export function ClusterPage() {
  const cluster = useClusterStatus();
  const now = useNow(30_000);
  const data = cluster.data;
  const healthy = data?.nodes.filter((n) => n.healthy).length ?? 0;
  const total = data?.nodes.length ?? 0;
  const leader = data?.nodes.find((n) => n.role === 'leader');

  return (
    <div className="screen">
      <PageHeader
        title="Cluster & replication"
        description="The Hangar nodes serving this registry. They share one database and one blob store, so any node answers any request."
        actions={
          <Link to="/system/status" className="button button--secondary">
            System status
          </Link>
        }
      />

      <Panel title="Nodes" titleId="cluster-nodes-title" aria-busy={cluster.isPending || undefined}>
        {cluster.isError ? (
          isMissingEndpoint(cluster.error) ? (
            <p className="meta">This server does not list its nodes yet. Upgrade Hangar to see them here.</p>
          ) : (
            <ErrorNote what="cluster nodes" error={cluster.error} privilege="system:cluster:read" onRetry={() => void cluster.refetch()} />
          )
        ) : !data ? (
          <p className="meta">Loading nodes…</p>
        ) : (
          <>
            <p className="cluster-summary">
              <StatusDot
                tone={healthy === total && total > 0 ? 'ok' : healthy === 0 ? 'bad' : 'warn'}
                halo
                label={<strong>{healthy === total ? `${total === 1 ? 'The node is' : `All ${total} nodes are`} healthy` : `${healthy} of ${total} nodes healthy`}</strong>}
              />
              <span className="meta">
                {total > 1 && (data.quorum ? 'Quorum held' : 'No quorum: scheduled work is paused')}
                {total > 1 && leader && ' · '}
                {leader && `${leader.name} runs scheduled tasks`}
              </span>
            </p>
            {total === 0 ? (
              <p className="meta">No node has reported a heartbeat yet.</p>
            ) : (
              <ul className="node-grid" aria-label="Cluster nodes">
                {data.nodes.map((n) => (
                  <NodeCard key={n.id} n={n} now={now} />
                ))}
              </ul>
            )}
          </>
        )}
      </Panel>

      <Panel
        title={
          <>
            Replication <Pill tone="neutral">Planned</Pill>
          </>
        }
        titleId="replication-title"
      >
        <p className="meta">
          Copying repositories to edge or disaster-recovery sites is not available yet. For availability today, run two or more nodes on shared,
          highly available PostgreSQL and storage.{' '}
          <a href={DOCS.highAvailability} target="_blank" rel="noreferrer">
            High availability guide
          </a>
        </p>
      </Panel>
    </div>
  );
}
