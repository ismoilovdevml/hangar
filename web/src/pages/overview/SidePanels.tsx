import { Link } from '@tanstack/react-router';
import { useClientEndpoints, useUpstreamHealth } from '../../api/hooks';
import type { UpstreamState } from '../../api/types';
import { CodeBlock } from '../../components/CodeBlock';
import { Panel } from '../../components/Panel';
import { clientSnippet } from '../../lib/clientSnippets';
import { ErrorNote } from '../../components/ErrorNote';

const UPSTREAM: Record<UpstreamState, { label: string; tone: 'ok' | 'warn' | 'bad' | 'neutral' }> = {
  healthy: { label: 'Healthy', tone: 'ok' },
  slow: { label: 'Slow', tone: 'warn' },
  blocked: { label: 'Blocked', tone: 'bad' },
  offline: { label: 'Offline', tone: 'bad' },
  unknown: { label: 'Unknown', tone: 'neutral' },
};

export function UpstreamSources() {
  const q = useUpstreamHealth();
  return (
    <Panel title="Upstream sources" titleId="upstreams-title" className="side-panel">
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
                <span className="upstreams__lat">
                  {u.latency_ms === null ? (
                    <>
                      <span aria-hidden="true">—</span>
                      <span className="visually-hidden">no recent latency</span>
                    </>
                  ) : (
                    `${u.latency_ms} ms`
                  )}
                </span>
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
        <CodeBlock label="client setup commands" lines={clientSnippet(q.data.items)} />
      )}
    </Panel>
  );
}

