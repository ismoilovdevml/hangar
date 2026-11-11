import { versionText } from '../../lib/format';
import { LEVEL_TONE, useSystemHealth } from '../../api/health';
import { useSystemStatus } from '../../api/hooks';
import { isMissingEndpoint, useCanSystem, useClusterStatus } from '../../api/ops';
import type { ClusterNode, DependencyStatus, SystemStatusDetail } from '../../api/opsTypes';
import { CopyButton } from '../../components/CopyButton';
import { DataTable, type Column } from '../../components/DataTable';
import { ErrorNote } from '../../components/ErrorNote';
import { NotAvailable } from '../../components/NotAvailable';
import { PageHeader } from '../../components/PageHeader';
import { Panel } from '../../components/Panel';
import { Pill, StatusDot } from '../../components/Pill';
import { DOCS } from '../../lib/docs';
import { dependencyLabel, formatUptime, hostOf } from '../../lib/system';
import { formatUtc } from '../../lib/tasks';

const STATE_LABEL = { ok: 'Operational', degraded: 'Degraded', down: 'Down', unknown: 'Unknown' } as const;
const LEVEL_LABEL = { healthy: 'Healthy', degraded: 'Degraded', down: 'Down' } as const;
/** Always listed; the other checks only while they fail (System status lists them all). */
const CORE_DEPENDENCIES = new Set(['database', 'blobstore', 'blobstores', 'eventbus']);

function Endpoint({ label, value, hint }: { label: string; value?: string; hint?: string }) {
  return (
    <div className="endpoint">
      <dt>{label}</dt>
      <dd>
        {value ? (
          <>
            <code className="endpoint__value">{value}</code>
            <CopyButton text={value} label={`Copy ${label.toLowerCase()}`} />
          </>
        ) : (
          <span className="meta">{hint ?? 'Not reported by this server'}</span>
        )}
      </dd>
    </div>
  );
}

/** Where the configuration lives: the server reads it at start-up and does not expose it over the API. */
function ConfigPanel() {
  return (
    <Panel title="Configuration" titleId="config-title">
      <p className="meta">
        Hangar reads <code className="inline-code">hangar.yaml</code> and <code className="inline-code">HANGAR_*</code> environment variables when it
        starts; change them on the host and restart. Sign-in and outbound proxies are edited under Settings.{' '}
        <a href={DOCS.configuration} target="_blank" rel="noreferrer">
          Configuration reference
        </a>
      </p>
    </Panel>
  );
}

export function SystemPage() {
  const status = useSystemStatus();
  const health = useSystemHealth();
  const cluster = useClusterStatus();
  const canReadSettings = useCanSystem('settings', 'read');
  const sys = status.data as SystemStatusDetail | undefined;

  const baseUrl = sys?.base_url || undefined;
  const registryHost = sys?.registry_host ?? hostOf(baseUrl);

  const depColumns: Column<DependencyStatus>[] = [
    { key: 'name', header: 'Dependency', className: 'col-first', cell: (d) => <strong>{dependencyLabel(d.name)}</strong> },
    {
      key: 'status',
      header: 'Status',
      className: 'nowrap',
      cell: (d) => {
        // The level (degraded or down) is in the health problems; the dependency only says ok or fail.
        const level = d.status === 'ok' ? 'healthy' : (health.problems.find((p) => p.name === d.name)?.status ?? 'down');
        return <StatusDot tone={LEVEL_TONE[level]} label={LEVEL_LABEL[level]} />;
      },
    },
    { key: 'latency', priority: 3, header: 'Latency', align: 'right', className: 'muted nowrap', cell: (d) => `${d.latency_ms} ms` },
    { key: 'error', header: 'Error', className: 'muted col-last', cell: (d) => d.error ?? '' },
  ];

  const nodeColumns: Column<ClusterNode>[] = [
    {
      key: 'name',
      header: 'Node',
      className: 'col-first',
      cell: (n) => (
        <div className="task-name">
          <strong>{n.name}</strong>
          <span className="task-name__sub">{n.address}</span>
        </div>
      ),
    },
    { key: 'role', priority: 2, header: 'Role', cell: (n) => <Pill tone={n.role === 'leader' ? 'info' : 'neutral'}>{n.role}</Pill> },
    { key: 'zone', priority: 3, header: 'Zone', className: 'muted', cell: (n) => n.zone ?? <NotAvailable /> },
    { key: 'version', priority: 3, header: 'Version', className: 'muted', cell: (n) => (n.version ? versionText(n.version) : <NotAvailable />) },
    {
      key: 'health',
      header: 'Health',
      className: 'nowrap',
      cell: (n) => <StatusDot tone={n.healthy ? 'ok' : 'bad'} label={n.healthy ? 'Healthy' : 'Unhealthy'} />,
    },
    { key: 'uptime', priority: 3, header: 'Uptime', className: 'muted nowrap', cell: (n) => (n.uptime_seconds !== undefined ? formatUptime(n.uptime_seconds) : <NotAvailable />) },
    { key: 'beat', priority: 2, header: 'Last heartbeat', className: 'muted nowrap col-last', cell: (n) => formatUtc(n.last_heartbeat, { seconds: true }) },
  ];

  return (
    <div className="screen">
      <PageHeader title="System" description="Health, version and configuration of this Hangar installation." />

      {status.isError ? (
        <ErrorNote what="system status" error={status.error} onRetry={() => void status.refetch()} />
      ) : (
        <Panel className="system-status" aria-label="System status" aria-busy={status.isPending || undefined}>
          {sys ? (
            <>
              <div className="system-status__state">
                <StatusDot tone={health.tone} halo={health.tone !== 'neutral'} label={<strong>{STATE_LABEL[health.state]}</strong>} />
                <span className="meta">{health.text}</span>
              </div>
              <dl className="facts">
                <div>
                  <dt>Instance</dt>
                  <dd>{sys.instance_name}</dd>
                </div>
                <div>
                  <dt>Version</dt>
                  <dd>
                    {versionText(sys.version)}
                    {sys.build?.commit && <span className="meta"> · {sys.build.commit.slice(0, 12)}</span>}
                  </dd>
                </div>
                <div>
                  <dt>Built</dt>
                  <dd>
                    {sys.build?.date ? formatUtc(sys.build.date) : <NotAvailable />}
                    {sys.build?.go_version && <span className="meta"> · {sys.build.go_version}</span>}
                  </dd>
                </div>
                <div>
                  <dt>Uptime</dt>
                  <dd>{sys.uptime_seconds !== undefined ? formatUptime(sys.uptime_seconds) : <NotAvailable />}</dd>
                </div>
                <div>
                  <dt>Nodes</dt>
                  <dd>
                    {sys.nodes_up} of {sys.nodes_total} up
                  </dd>
                </div>
                <div>
                  <dt>Web UI</dt>
                  <dd>{sys.web_ui === undefined ? <NotAvailable /> : sys.web_ui ? 'Embedded in the binary' : 'Served separately'}</dd>
                </div>
              </dl>
            </>
          ) : (
            <p className="meta">Checking system status…</p>
          )}
        </Panel>
      )}

      <div className="system-grid">
        <Panel title="Endpoints" titleId="endpoints-title">
          <dl className="endpoints">
            <Endpoint label="Base URL" value={baseUrl} hint={sys ? 'Not set: clients use the address they reached' : undefined} />
            <Endpoint label="Registry host" value={registryHost} hint="Derived from the base URL" />
            <Endpoint label="API" value={baseUrl ? `${baseUrl}/api/v1` : undefined} />
          </dl>
          <p className="meta">Docker clients log in to the registry host; every other format uses the base URL.</p>
        </Panel>

        <Panel flush title="Dependencies" titleId="deps-title">
          <DataTable
            caption="Dependency health"
            columns={depColumns}
            rows={(sys?.dependencies ?? []).filter((d) => CORE_DEPENDENCIES.has(d.name) || d.status !== 'ok')}
            rowKey={(d) => d.name}
            minWidth={460}
            empty={status.isPending ? 'Loading…' : 'This server reports no dependency checks.'}
          />
        </Panel>
      </div>

      <Panel flush title="Nodes" titleId="nodes-title" action={cluster.data && <Pill tone={cluster.data.quorum ? 'ok' : 'bad'}>{cluster.data.quorum ? 'Quorum' : 'No quorum'}</Pill>}>
        {cluster.isError ? (
          isMissingEndpoint(cluster.error) ? (
            <p className="meta panel-note">
              This server does not list cluster nodes yet{sys ? `; it reports ${sys.nodes_up} of ${sys.nodes_total} up` : ''}.
            </p>
          ) : (
            <ErrorNote what="cluster nodes" error={cluster.error} privilege="system:cluster:read" onRetry={() => void cluster.refetch()} />
          )
        ) : (
          <DataTable
            caption="Cluster nodes"
            columns={nodeColumns}
            rows={cluster.data?.nodes ?? []}
            rowKey={(n) => n.id}
            minWidth={860}
            empty={cluster.isPending ? 'Loading…' : 'No nodes reported.'}
          />
        )}
      </Panel>

      {canReadSettings && <ConfigPanel />}
    </div>
  );
}
