import { screen, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import type { OverviewMetrics } from '../api/types';
import { overview } from '../mocks/data';
import { server } from '../mocks/node';
import { renderApp } from './render';

/** A fresh server: no quotas, no request statistics, no proxy cache metrics, nothing stored. */
function bareMetrics(): OverviewMetrics {
  const m = overview('today');
  return {
    ...m,
    storage: { used_bytes: 0, capacity_bytes: null, added_bytes_24h: 0 },
    requests: { total: null, trend_pct: null, peak_total: null, p95_ms: null, coverage_start: null },
    cache: { hit_ratio: null, bytes_saved: null, hits: null, misses: null },
    issues: { critical: 0, packages_affected: 0, quarantined: 0, open_findings: 0 },
    traffic: m.traffic.map((b) => ({ ...b, downloads: null, uploads: 0 })),
    storage_by_format: [],
  };
}

const useMetrics = (m: OverviewMetrics) =>
  server.use(http.get('*/api/v1/metrics/overview', () => HttpResponse.json(m)));

describe('overview with metrics the server does not report', () => {
  it('shows dashes and explanations instead of invented numbers', async () => {
    useMetrics(bareMetrics());
    renderApp('/');
    const kpis = await screen.findByRole('region', { name: 'Key metrics' });

    await within(kpis).findByText('B · no capacity limit set');
    const storage = within(kpis).getByRole('article', { name: 'Storage used' });
    expect(within(storage).getByText('0')).toBeInTheDocument();
    expect(within(storage).queryByRole('meter')).not.toBeInTheDocument();

    const requests = within(kpis).getByRole('article', { name: 'Requests today' });
    expect(within(requests).getByText('—')).toBeInTheDocument();
    expect(within(requests).getByText('Not reported')).toBeInTheDocument();
    expect(within(requests).getByText('This server does not report request statistics.')).toBeInTheDocument();
    expect(within(requests).queryByRole('meter')).not.toBeInTheDocument();

    const cache = within(kpis).getByRole('article', { name: 'Served from cache' });
    expect(within(cache).getByText('—')).toBeInTheDocument();
    expect(within(cache).getByText('This server does not report proxy cache statistics.')).toBeInTheDocument();
    expect(within(cache).queryByText('Healthy')).not.toBeInTheDocument();

    expect(screen.getByText('Downloads (not recorded)')).toBeInTheDocument();
    expect(screen.getByRole('img', { name: /Download counts are not recorded/ })).toBeInTheDocument();
    expect(screen.getByText(/Nothing stored yet/)).toBeInTheDocument();

    // Nothing leaks through as a literal null/NaN/undefined.
    expect(document.body.textContent).not.toMatch(/NaN|null|undefined|Infinity/);
  });

  it('keeps known numbers when only some are missing, and says when counting started', async () => {
    const m = overview('today');
    const restart = new Date(new Date(m.generated_at).getTime() - 10_000);
    useMetrics({
      ...m,
      requests: { ...m.requests, trend_pct: null, p95_ms: null, coverage_start: restart.toISOString() },
      cache: { hit_ratio: 0.5, bytes_saved: null, hits: 6, misses: 6 },
      traffic: m.traffic.map((b, i) => (i < 3 ? { ...b, downloads: null } : b)),
    });
    renderApp('/');
    await screen.findByText('18.4M');
    const requests = screen.getByRole('article', { name: 'Requests today' });
    expect(within(requests).queryByText(/%$/)).not.toBeInTheDocument();
    expect(within(requests).getByText(/^Counted since \d\d:\d\d$/)).toBeInTheDocument();

    const cache = screen.getByRole('article', { name: 'Served from cache' });
    expect(within(cache).getByText('50.0')).toBeInTheDocument();
    expect(within(cache).getByText('Low')).toBeInTheDocument();
    expect(within(cache).getByText('6 of 12 requests')).toBeInTheDocument();

    const table = screen.getByRole('table', { name: 'Traffic per interval' });
    expect(within(table).getAllByText('not recorded')).toHaveLength(3);
    const chart = screen.getByRole('region', { name: 'Traffic today' });
    expect(within(chart).getByText('Downloads', { selector: '.legend__item' })).toBeInTheDocument();
  });

  it('says when no proxy served content in the range', async () => {
    const m = overview('1h');
    useMetrics({ ...m, cache: { hit_ratio: null, bytes_saved: 0, hits: 0, misses: 0 } });
    renderApp('/?range=1h');
    await screen.findByText('No proxy repository served content in this range.');
    const cache = screen.getByRole('article', { name: 'Served from cache' });
    expect(within(cache).getByText('—')).toBeInTheDocument();
    expect(within(cache).getByText('No proxy requests in this range')).toBeInTheDocument();
    expect(within(cache).queryByRole('meter')).not.toBeInTheDocument();
  });

  it('explains empty upstream and client lists', async () => {
    server.use(
      http.get('*/api/v1/upstreams/health', () => HttpResponse.json({ items: [], next_cursor: null })),
      http.get('*/api/v1/me/client-endpoints', () => HttpResponse.json({ items: [], next_cursor: null })),
    );
    renderApp('/');
    expect(await screen.findByText(/No proxy repositories yet/)).toBeInTheDocument();
    expect(await screen.findByText(/No repositories you can read yet/)).toBeInTheDocument();
  });
});
