import { useId, useState } from 'react';
import { useSystemStatus } from '../../api/hooks';
import { isMissingEndpoint, useCanSystem, useClusterStatus, useSystemConfig } from '../../api/ops';
import type { ClusterNode, DependencyStatus, SystemStatusDetail } from '../../api/opsTypes';
import { CopyButton } from '../../components/CopyButton';
import { DataTable, type Column } from '../../components/DataTable';
import { ErrorNote } from '../../components/ErrorNote';
import { IconSearch } from '../../components/icons';
import { NotAvailable } from '../../components/NotAvailable';
import { PageHeader } from '../../components/PageHeader';
import { Panel } from '../../components/Panel';
import { Pill, StatusDot } from '../../components/Pill';
import { configValue, dependencyLabel, flattenConfig, formatUptime, hostOf } from '../../lib/system';
import { formatUtc } from '../../lib/tasks';

const STATE_TONE = { ok: 'ok', degraded: 'warn', down: 'bad' } as const;
const STATE_LABEL = { ok: 'Operational', degraded: 'Degraded', down: 'Down' } as const;

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

function ConfigPanel() {
  const config = useSystemConfig(true);
  const [filter, setFilter] = useState('');
  const id = useId();
  if (config.isError) {
    return (
      <Panel title="Effective configuration" titleId="config-title">
        {isMissingEndpoint(config.error) ? (
          <p className="meta">This server does not expose its effective configuration yet. Check hangar.yaml and the HANGAR_* environment on the host.</p>
        ) : (
          <ErrorNote what="the configuration" onRetry={() => void config.refetch()} />
        )}
      </Panel>
    );
  }
  const q = filter.trim().toLowerCase();
  const sections = config.data
    ? flattenConfig(config.data)
        .map((s) => ({ ...s, entries: s.entries.filter((e) => !q || e.key.toLowerCase().includes(q) || (!e.secret && e.value.toLowerCase().includes(q))) }))
        .filter((s) => s.entries.length)
    : [];
  return (
    <Panel
      title="Effective configuration"
      titleId="config-title"
      action={
        <div className="filter-search">
          <label htmlFor={id} className="visually-hidden">
            Filter settings
          </label>
          <IconSearch size={16} />
          <input
            id={id}
            className="filter-search__input"
            type="search"
            placeholder="Filter settings"
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            autoComplete="off"
            spellCheck={false}
          />
        </div>
      }
    >
      <p className="meta">Read-only. Values come from hangar.yaml, HANGAR_* environment variables or built-in defaults. Secrets are never shown.</p>
      {config.isPending && <p className="meta">Loading…</p>}
      {config.data && sections.length === 0 && <p className="meta">No setting matches “{filter}”.</p>}
      {sections.map((s) => (
        <section key={s.name} className="config-section" aria-label={`${s.name} settings`}>
          <h3 className="section-label">{s.name}</h3>
          <dl className="config-list">
            {s.entries.map((e) => (
              <div key={e.key} className="config-list__row">
                <dt>
                  <code>{e.key}</code>
                </dt>
                <dd>
                  {e.secret ? (
                    <span className="config-secret">
                      <span aria-hidden="true">{e.value}</span>
                      <span className="visually-hidden">hidden</span> <Pill tone="neutral">secret</Pill>
                    </span>
                  ) : e.value ? (
                    <code className="config-value">{e.value}</code>
                  ) : (
                    <span className="meta">empty</span>
                  )}
                  {e.source && <span className="config-source">{e.source === 'env' ? 'environment' : e.source}</span>}
                </dd>
              </div>
            ))}
          </dl>
        </section>
      ))}
    </Panel>
  );
}

export function SystemPage() {
  const status = useSystemStatus();
  const cluster = useClusterStatus();
  const canReadSettings = useCanSystem('settings', 'read');
  // Base URL may come from the status or, for admins, from the configuration.
  const config = useSystemConfig(canReadSettings);
  const sys = status.data as SystemStatusDetail | undefined;

  const baseUrl = sys?.base_url ?? (configValue(config.data, 'server.base_url') as string | undefined);
  const registryHost = sys?.registry_host ?? hostOf(baseUrl);

  const depColumns: Column<DependencyStatus>[] = [
    { key: 'name', header: 'Dependency', className: 'col-first', cell: (d) => <strong>{dependencyLabel(d.name)}</strong> },
    {
      key: 'status',
      header: 'Status',
      className: 'nowrap',
      cell: (d) => <StatusDot tone={d.status === 'ok' ? 'ok' : 'bad'} label={d.status === 'ok' ? 'Healthy' : 'Failing'} />,
    },
    { key: 'latency', header: 'Latency', align: 'right', className: 'muted nowrap', cell: (d) => `${d.latency_ms} ms` },
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
    { key: 'role', header: 'Role', cell: (n) => <Pill tone={n.role === 'leader' ? 'info' : 'neutral'}>{n.role}</Pill> },
    { key: 'zone', header: 'Zone', className: 'muted', cell: (n) => n.zone ?? <NotAvailable /> },
    { key: 'version', header: 'Version', className: 'muted', cell: (n) => n.version ?? <NotAvailable /> },
    {
      key: 'health',
      header: 'Health',
      className: 'nowrap',
      cell: (n) => <StatusDot tone={n.healthy ? 'ok' : 'bad'} label={n.healthy ? 'Healthy' : 'Unhealthy'} />,
    },
    { key: 'uptime', header: 'Uptime', className: 'muted nowrap', cell: (n) => (n.uptime_seconds !== undefined ? formatUptime(n.uptime_seconds) : <NotAvailable />) },
    { key: 'beat', header: 'Last heartbeat', className: 'muted nowrap col-last', cell: (n) => formatUtc(n.last_heartbeat, { seconds: true }) },
  ];

  return (
    <div className="screen">
      <PageHeader title="System" description="Health, version and configuration of this Hangar installation." />

      {status.isError ? (
        <ErrorNote what="system status" onRetry={() => void status.refetch()} />
      ) : (
        <Panel className="system-status" aria-label="System status" aria-busy={status.isPending || undefined}>
          {sys ? (
            <>
              <div className="system-status__state">
                <StatusDot tone={STATE_TONE[sys.state]} halo label={<strong>{STATE_LABEL[sys.state]}</strong>} />
                <span className="meta">{sys.message}</span>
              </div>
              <dl className="facts">
                <div>
                  <dt>Instance</dt>
                  <dd>{sys.instance_name}</dd>
                </div>
                <div>
                  <dt>Version</dt>
                  <dd>
                    {sys.version}
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
            <Endpoint label="Base URL" value={baseUrl} hint={canReadSettings ? undefined : 'Shown to administrators'} />
            <Endpoint label="Registry host" value={registryHost} hint="Derived from the base URL" />
            <Endpoint label="API" value={baseUrl ? `${baseUrl}/api/v1` : undefined} />
          </dl>
          <p className="meta">Docker clients log in to the registry host; every other format uses the base URL.</p>
        </Panel>

        <Panel flush title="Dependencies" titleId="deps-title">
          <DataTable
            caption="Dependency health"
            columns={depColumns}
            rows={sys?.dependencies ?? []}
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
            <ErrorNote what="cluster nodes" onRetry={() => void cluster.refetch()} />
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
