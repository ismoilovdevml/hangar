import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import type { OverviewMetrics } from '../api/types';
import { overview, session, systemStatus } from '../mocks/data';
import { server } from '../mocks/node';
import { renderApp } from './render';

/** A fresh server: no quotas, no request statistics, no proxy cache metrics, nothing stored or scanned. */
function bareMetrics(): OverviewMetrics {
  const m = overview('today');
  return {
    ...m,
    summary: { published_since_yesterday: 0, failed_pushes: 0 },
    activity: { published: 0, cached: 0, pushes_rejected: 0 },
    storage: { used_bytes: 0, capacity_bytes: null, added_bytes_24h: 0 },
    requests: { total: null, trend_pct: null, peak_total: null, p95_ms: null, coverage_start: null, server_errors: null, denied: null },
    cache: { hit_ratio: null, bytes_saved: null, hits: null, misses: null },
    issues: { critical: 0, packages_affected: 0, quarantined: 0, open_findings: 0 },
    traffic: m.traffic.map((b) => ({ ...b, downloads: null, requests: null, uploads: 0 })),
    storage_by_format: [],
  };
}

/** Everything in order: nothing for the to-do list. */
function calmMetrics(): OverviewMetrics {
  const m = overview('today');
  return {
    ...m,
    activity: { ...m.activity, pushes_rejected: 0 },
    issues: { ...m.issues, critical: 0, packages_affected: 0, quarantined: 0 },
  };
}

const useMetrics = (m: OverviewMetrics) => server.use(http.get('*/api/v1/metrics/overview', () => HttpResponse.json(m)));
const fail = (path: string) =>
  server.use(http.get(`*/api/v1${path}`, () => HttpResponse.json({ code: 'internal', message: 'boom' }, { status: 500 })));

describe('overview with metrics the server does not report', () => {
  it('shows dashes and explanations instead of invented numbers', async () => {
    useMetrics(bareMetrics());
    renderApp('/');
    const today = await screen.findByRole('region', { name: /^Today · / });
    expect(await within(today).findByText(/packages cached/)).toHaveTextContent('0 packages cached, 0 published, 0 pushes rejected.');
    expect(within(today).getByText('This server does not report request statistics.')).toBeInTheDocument();
    expect(within(today).getByText('Requests per interval are not recorded for this range.')).toBeInTheDocument();

    const outcomes = screen.getByRole('region', { name: 'Request outcomes' });
    expect(within(outcomes).getByRole('img', { name: 'Request outcomes are not reported by this server' })).toHaveTextContent('—');
    expect(within(outcomes).getByText('No request statistics')).toBeInTheDocument();

    const storage = screen.getByRole('link', { name: /^Storage/ });
    expect(within(storage).getByText('0 B')).toBeInTheDocument();
    expect(within(storage).getByText('Nothing stored yet · no quota')).toBeInTheDocument();

    expect(within(screen.getByRole('link', { name: /^Security/ })).getByText('Clean')).toBeInTheDocument();

    // No quota yet: the admin is pointed at the blob stores.
    const todo = screen.getByRole('region', { name: 'To do' });
    expect(await within(todo).findByRole('link', { name: /Set a storage quota/ })).toHaveAttribute('href', '/blob-stores');

    // Nothing leaks through as a literal null/NaN/undefined.
    expect(document.body.textContent).not.toMatch(/NaN|null|undefined|Infinity/);
  });

  it('says when counting started and when there were no requests yet', async () => {
    const m = overview('today');
    const restart = new Date(new Date(m.generated_at).getTime() - 10_000);
    useMetrics({
      ...m,
      requests: { ...m.requests, total: 0, p95_ms: null, server_errors: 0, denied: 0, coverage_start: restart.toISOString() },
      traffic: m.traffic.map((b, i) => (i < 3 ? { ...b, requests: null, downloads: null } : { ...b, requests: 0 })),
    });
    renderApp('/');
    expect(await screen.findByRole('region', { name: /^Today · .* · Counted since \d\d:\d\d$/ })).toBeInTheDocument();
    const outcomes = screen.getByRole('region', { name: 'Request outcomes' });
    expect(within(outcomes).getByRole('img', { name: 'No requests in this range yet' })).toBeInTheDocument();
    expect(within(outcomes).getByText('Nothing to measure yet')).toBeInTheDocument();
    expect(screen.getByRole('img', { name: /^Requests per hour: none yet/ })).toBeInTheDocument();
    expect(within(screen.getByRole('table', { name: 'Requests per hour' })).getAllByText('not recorded')).toHaveLength(3);
  });

  it('says when scanning is off, and asks to turn it on', async () => {
    const m = overview('today');
    useMetrics({ ...m, issues: { ...m.issues, critical: 0, scanning: { enabled: false, scanned_components: 0, last_scan_at: null } } });
    renderApp('/');
    const security = await screen.findByRole('link', { name: /^Security/ });
    expect(await within(security).findByText('Scanning off')).toBeInTheDocument();
    expect(within(security).queryByText('Clean')).not.toBeInTheDocument();
    const todo = screen.getByRole('region', { name: 'To do' });
    expect(within(todo).getByRole('link', { name: /Turn on vulnerability scanning/ })).toBeInTheDocument();
  });

  it('shows an all-clear to-do list when nothing needs attention', async () => {
    useMetrics(calmMetrics());
    server.use(
      http.get('*/api/v1/auth/session', () => HttpResponse.json({ ...session, token_warnings: [] })),
      http.get('*/api/v1/upstreams/health', () =>
        HttpResponse.json({
          items: [{ name: 'nuget-proxy', url: 'https://api.nuget.org/v3/index.json', state: 'healthy', latency_ms: 140, requests: 52 }],
          next_cursor: null,
        }),
      ),
    );
    renderApp('/');
    const todo = await screen.findByRole('region', { name: 'To do' });
    expect(await within(todo).findByText('Nothing needs you right now')).toBeInTheDocument();
    expect(todo.querySelector('.todo__count')).toBeNull();
    const upstream = screen.getByRole('link', { name: /^Upstream/ });
    expect(within(upstream).getByText('api.nuget.org')).toBeInTheDocument();
    expect(within(upstream).getByText('140 ms · cache hit 94%')).toBeInTheDocument();
  });

  it('turns a degraded system and a failed backup into to-do items', async () => {
    useMetrics(calmMetrics());
    server.use(
      http.get('*/api/v1/system/status', () =>
        HttpResponse.json({ ...systemStatus, state: 'down', message: 'Database unreachable', nodes_up: 1 }),
      ),
    );
    renderApp('/');
    const todo = await screen.findByRole('region', { name: 'To do' });
    const outage = await within(todo).findByRole('link', { name: /Database unreachable/ });
    expect(outage).toHaveAttribute('href', '/system/status');
    expect(outage).toHaveTextContent('OUTAGE');
    expect(outage).toHaveTextContent('1 of 3 nodes up');
  });

  it('explains empty upstream and download lists', async () => {
    server.use(
      http.get('*/api/v1/upstreams/health', () => HttpResponse.json({ items: [], next_cursor: null })),
      http.get('*/api/v1/search', () =>
        HttpResponse.json({ items: [], next_cursor: null, total: 0, facets: { format: [], security: [], repository: [] } }),
      ),
    );
    renderApp('/');
    const upstream = await screen.findByRole('link', { name: /^Upstream/ });
    expect(await within(upstream).findByText('No proxy yet')).toBeInTheDocument();
    expect(upstream).toHaveAttribute('href', '/repositories/new?type=proxy');
    const top = screen.getByRole('region', { name: 'Most downloaded' });
    expect(await within(top).findByText(/No downloads yet/)).toBeInTheDocument();
  });

  it('offers a retry on every card when the APIs fail', async () => {
    fail('/metrics/overview');
    fail('/upstreams/health');
    fail('/search');
    const user = userEvent.setup();
    renderApp('/');
    const today = await screen.findByRole('region', { name: /^Today · / });
    expect(await within(today).findByText("Could not load today's activity.")).toBeInTheDocument();
    expect(within(screen.getByRole('region', { name: 'To do' })).getByText('Could not load what needs attention.')).toBeInTheDocument();
    expect(screen.getByText('Request outcomes are unavailable.')).toBeInTheDocument();
    expect(await screen.findByText('Could not load upstream health.')).toBeInTheDocument();
    expect(await screen.findByText('Could not load the most downloaded packages.')).toBeInTheDocument();

    server.resetHandlers();
    await user.click(within(today).getByRole('button', { name: 'Retry' }));
    await waitFor(() => expect(within(today).getByText(/packages cached/)).toBeInTheDocument());
  });
});
