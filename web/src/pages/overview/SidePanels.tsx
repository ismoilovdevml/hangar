import { Link } from '@tanstack/react-router';
import { useClientEndpoints, useUpstreamHealth } from '../../api/hooks';
import type { OverviewRange, UpstreamHealth, UpstreamState } from '../../api/types';
import { ClientSetup } from '../../components/ClientSetup';
import { Panel } from '../../components/Panel';
import { ErrorNote } from '../../components/ErrorNote';
import { NotAvailable } from '../../components/NotAvailable';
import { formatCompact } from '../../lib/format';

const UPSTREAM: Record<UpstreamState, { label: string; tone: 'ok' | 'warn' | 'bad' | 'neutral' }> = {
  healthy: { label: 'Healthy', tone: 'ok' },
  slow: { label: 'Slow', tone: 'warn' },
  blocked: { label: 'Blocked', tone: 'bad' },
  offline: { label: 'Offline', tone: 'bad' },
  unknown: { label: 'Unknown', tone: 'neutral' },
};

/** How the latency column reads for each range. */
const LATENCY_RANGE: Record<OverviewRange, string> = {
  '1h': 'last hour',
  today: 'today',
  '7d': '7 days',
  '30d': '30 days',
};

function Latency({ u }: { u: UpstreamHealth }) {
  if (u.latency_ms === null) {
    const reason = u.requests === null ? 'Latency not reported' : 'No remote requests in this range';
    return (
      <span className="upstreams__lat" title={reason}>
        <NotAvailable reason={reason} />
      </span>
    );
  }
  const n = u.requests ?? 0;
  return (
    <span className="upstreams__lat" title={`Median of ${formatCompact(n)} remote ${n === 1 ? 'request' : 'requests'}`}>
      {u.latency_ms} ms
    </span>
  );
}

export function UpstreamSources({ range }: { range: OverviewRange }) {
  const q = useUpstreamHealth(range);
  return (
    <Panel
      title="Upstream sources"
      titleId="upstreams-title"
      className="side-panel"
      action={<span className="meta">Median latency · {LATENCY_RANGE[range]}</span>}
    >
      {q.isError ? (
        <ErrorNote what="upstream health" onRetry={() => void q.refetch()} />
      ) : q.isPending ? (
        <p className="meta">Checking upstreams…</p>
      ) : q.data.items.length === 0 ? (
        <p className="meta">
          No proxy repositories yet. <Link to="/repositories/new" search={{ type: 'proxy' }}>Add one</Link> to cache a public registry.
        </p>
      ) : (
        <ul className="upstreams">
          {q.data.items.map((u) => {
            const s = UPSTREAM[u.state];
            return (
              <li key={u.name} className="upstreams__row">
                <span className={`status__dot dot-${s.tone}`} aria-hidden="true" />
                <span className="upstreams__name" title={u.url}>
                  {u.name}
                </span>
                <Latency u={u} />
                <span className={`pill tone-${s.tone}`}>{s.label}</span>
              </li>
            );
          })}
        </ul>
      )}
    </Panel>
  );
}

export function ConnectClient() {
  const q = useClientEndpoints();
  return (
    <Panel title="Connect a client" titleId="connect-title" className="side-panel side-panel--tight">
      {q.isError ? (
        <ErrorNote what="client endpoints" onRetry={() => void q.refetch()} />
      ) : q.isPending ? (
        <p className="meta">Loading endpoints…</p>
      ) : q.data.items.length === 0 ? (
        <p className="meta">
          No repositories you can read yet. <Link to="/repositories/new">Create one</Link> to get client settings.
        </p>
      ) : (
        <ClientSetup
          targets={q.data.items.map((e) =>
            e.format === 'docker' ? { format: e.format, repository: e.repository, host: e.endpoint } : { format: e.format, repository: e.repository, url: e.endpoint },
          )}
        />
      )}
    </Panel>
  );
}

