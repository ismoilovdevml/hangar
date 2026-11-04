import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { session } from '../mocks/data';
import { server } from '../mocks/node';
import { systemMockState } from '../mocks/system';
import {
  appendRecords,
  inlineAttrs,
  offsets,
  rowHeight,
  period,
  shortTime,
  summarize,
  toJsonLines,
  versionText,
  visibleRange,
  ROW_HEIGHT,
  type LogRecord,
  type SystemNode,
} from '../pages/system/model';
import { renderApp } from './render';

const search = (h: { location: { search: string } }) => new URLSearchParams(h.location.search);

const withPrivileges = (privileges: string[]) =>
  server.use(http.get('*/api/v1/auth/session', () => HttpResponse.json({ ...session, privileges })));

describe('system status', () => {
  it('shows a single healthy node as a calm checklist', async () => {
    const user = userEvent.setup();
    systemMockState.singleNode();
    renderApp('/system/status');
    expect(await screen.findByRole('heading', { level: 1, name: 'Everything is working' })).toBeInTheDocument();
    const summary = screen.getByRole('region', { name: 'Summary' });
    expect(within(summary).getByText('7 of 7 checks passing · hangar-1 · v1.0.0')).toBeInTheDocument();

    const list = screen.getByRole('list', { name: 'Checks of hangar-1' });
    const rows = within(list).getAllByRole('button', { expanded: false });
    expect(rows.length).toBe(7);
    // Nothing is wrong, so every row starts closed; a single node has no own heading.
    expect(within(list.closest('section')!).queryByRole('heading')).not.toBeInTheDocument();

    const db = within(list).getByRole('button', { name: /^Database \(healthy\)/ });
    expect(db).toHaveTextContent('PostgreSQL 17.2 · 6 of 40 connections in use');
    expect(db).toHaveTextContent('2 ms');
    await user.click(db);
    expect(db).toHaveAttribute('aria-expanded', 'true');
    const detail = document.getElementById(db.getAttribute('aria-controls')!)!;
    expect(within(detail).getByText('Connections in use').nextSibling).toHaveTextContent('6 of 40');
    expect(within(detail).getByText('db.hangar.example.com:5432/hangar')).toBeInTheDocument();
    expect(detail).toHaveTextContent(/Healthy since \d+ \w{3} \d{2}:\d{2} · checked \d+ s ago/);
    expect(within(detail).getByRole('link', { name: 'Logs of hangar-1' })).toHaveAttribute('href', '/system/logs?node=hangar-1');

    const proxies = within(list).getByRole('button', { name: /^Upstream proxies \(healthy\)/ });
    await user.click(proxies);
    const pd = document.getElementById(proxies.getAttribute('aria-controls')!)!;
    expect(within(pd).getByText(/^Was degraded \d{2}:\d{2}.\d{2}:\d{2}$/)).toHaveAttribute('title', '2 of 3 healthy · 1 slow');
    expect(within(pd).getByRole('link', { name: 'Repositories' })).toBeInTheDocument();

    await user.click(db);
    expect(db).toHaveAttribute('aria-expanded', 'false');
    expect(screen.getByText(/Scheduler leader/)).toHaveTextContent('Scheduler leader');
    expect(screen.getByText('http://10.0.1.11:8080')).toBeInTheDocument();
  });

  it('groups several nodes and opens the checks that need attention', async () => {
    renderApp('/system/status');
    expect(await screen.findByRole('heading', { level: 1, name: '1 check needs attention' })).toBeInTheDocument();
    const summary = screen.getByRole('region', { name: 'Summary' });
    // The silent node's older version and checks do not count.
    expect(within(summary).getByText('13 of 14 checks passing · 2 of 3 nodes up · v1.0.0')).toBeInTheDocument();

    const one = screen.getByRole('region', { name: /^hangar-1/ });
    expect(within(one).getByText('This node')).toBeInTheDocument();
    expect(within(one).getByText('Scheduler leader')).toBeInTheDocument();
    expect(within(one).getByRole('button', { name: /^Task scheduler \(healthy\)/ })).toHaveTextContent('leader');

    const two = screen.getByRole('region', { name: /^hangar-2/ });
    const proxies = within(two).getByRole('button', { name: /^Upstream proxies \(degraded\)/ });
    expect(proxies).toHaveAttribute('aria-expanded', 'true');
    expect(within(two).getByText('Offline remotes').nextSibling).toHaveTextContent('maven-central');
    expect(within(two).getAllByRole('button', { expanded: true }).length).toBe(1);

    const three = screen.getByRole('region', { name: /^hangar-3/ });
    expect(within(three).getByText('Not responding')).toBeInTheDocument();
    expect(within(three).getByRole('note')).toHaveTextContent(/No heartbeat for 30 min/);
    expect(within(three).queryAllByRole('button', { expanded: true }).length).toBe(0);
  });

  it('formats a timestamp fact as a time', async () => {
    const user = userEvent.setup();
    systemMockState.singleNode();
    renderApp('/system/status');
    const row = await screen.findByRole('button', { name: /^Task scheduler \(healthy\)/ });
    await user.click(row);
    const detail = document.getElementById(row.getAttribute('aria-controls')!)!;
    const time = within(detail).getByText('Role since').nextSibling!.firstChild as HTMLElement;
    expect(time.tagName).toBe('TIME');
    expect(time).toHaveTextContent(/^\d+ \w{3} \d{2}:\d{2}$/);
    expect(within(detail).getByRole('link', { name: 'Scheduled tasks' })).toHaveAttribute('href', '/tasks');
  });

  it('hides the logs link without the cluster admin privilege', async () => {
    const user = userEvent.setup();
    withPrivileges(['system:cluster:read']);
    systemMockState.singleNode();
    renderApp('/system/status');
    const db = await screen.findByRole('button', { name: /^Database \(healthy\)/ });
    await user.click(db);
    const detail = document.getElementById(db.getAttribute('aria-controls')!)!;
    expect(within(detail).queryByRole('link')).not.toBeInTheDocument();
  });

  it('refreshes on demand and can pause auto-refresh', async () => {
    const user = userEvent.setup();
    renderApp('/system/status');
    await screen.findByRole('region', { name: 'Summary' });
    const controls = screen.getByRole('group', { name: 'Refresh status' });
    const before = systemMockState.nodesRequests;
    await user.click(within(controls).getByRole('button', { name: 'Refresh now' }));
    await waitFor(() => expect(systemMockState.nodesRequests).toBe(before + 1));

    await user.click(within(controls).getByRole('button', { name: 'Live · every 15 s – pause' }));
    expect(within(controls).getByRole('button', { name: 'Paused – resume' })).toBeInTheDocument();
    await user.click(within(controls).getByRole('button', { name: 'Paused – resume' }));
    expect(within(controls).getByRole('button', { name: 'Live · every 15 s – pause' })).toBeInTheDocument();
  });

  it('shows a skeleton while the first answer loads', async () => {
    server.use(http.get('*/api/v1/system/nodes', () => new Promise<Response>(() => {})));
    renderApp('/system/status');
    expect(await screen.findByRole('heading', { level: 1, name: 'Checking every node…' })).toBeInTheDocument();
  });

  it('explains the missing privilege instead of loading', async () => {
    withPrivileges(['repo:*:read']);
    renderApp('/system/status');
    expect(await screen.findByText(/needs the/)).toHaveTextContent('system:cluster:read');
    expect(systemMockState.nodesRequests).toBe(0);
  });

  it('shows the server error with a retry', async () => {
    server.use(http.get('*/api/v1/system/nodes', () => HttpResponse.json({ code: 'internal', message: 'internal error' }, { status: 500 })));
    renderApp('/system/status');
    expect(await screen.findByRole('alert')).toHaveTextContent('Could not load the system status.');
  });
});

describe('system logs', () => {
  afterEach(() => vi.restoreAllMocks());

  it('lists the newest records of the answering node with level colours', async () => {
    renderApp('/system/logs');
    const view = await screen.findByRole('region', { name: 'Log records of hangar-1' });
    expect(screen.getByText(/Showing 240 records · node hangar-1 keeps 240 of 5,000/)).toBeInTheDocument();
    const rows = within(view).getAllByRole('listitem');
    // Virtualised: only the rows near the bottom are rendered.
    expect(rows.length).toBeLessThan(60);
    expect(rows.at(-1)).toHaveAttribute('aria-posinset', '240');
    expect(view.querySelector('.log-row--error')).not.toBeNull();
    expect(screen.getByText(/docker compose logs hangar/)).toBeInTheDocument();
  });

  it('opens a record as JSON', async () => {
    const user = userEvent.setup();
    renderApp('/system/logs');
    const view = await screen.findByRole('region', { name: 'Log records of hangar-1' });
    const last = within(view).getAllByRole('listitem').at(-1)!;
    const line = within(last).getByRole('button');
    expect(line).toHaveAttribute('aria-expanded', 'false');
    await user.click(line);
    expect(line).toHaveAttribute('aria-expanded', 'true');
    const detail = document.getElementById(line.getAttribute('aria-controls')!)!;
    const json = JSON.parse(detail.textContent!) as Record<string, unknown>;
    expect(json).toMatchObject({ level: expect.any(String), msg: expect.any(String), request_id: expect.stringMatching(/^req-/) });
  });

  it('filters by level and text through the URL', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/system/logs');
    await screen.findByRole('region', { name: 'Log records of hangar-1' });

    await user.click(screen.getByRole('combobox', { name: /^Level/ }));
    await user.click(await screen.findByRole('option', { name: 'Errors only' }));
    await waitFor(() => expect(search(history).get('level')).toBe('error'));
    const view = await screen.findByRole('region', { name: 'Log records of hangar-1' });
    await waitFor(() => expect(screen.getByText(/Showing 30 records/)).toBeInTheDocument());
    for (const row of within(view).getAllByRole('listitem')) expect(row).toHaveClass('log-row--error');

    await user.type(screen.getByLabelText('Filter log records'), 'nothing-matches-this');
    await waitFor(() => expect(search(history).get('q')).toBe('nothing-matches-this'));
    expect(await screen.findByText(/^No records match these filters\./)).toBeInTheDocument();
  });

  it('switches nodes with the keyboard and reports an unreachable one', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/system/logs');
    await screen.findByRole('region', { name: 'Log records of hangar-1' });
    const picker = screen.getByRole('combobox', { name: /^Node/ });
    picker.focus();
    await user.keyboard('{ArrowDown}');
    expect(picker).toHaveAttribute('aria-expanded', 'true');
    await user.keyboard('{ArrowDown}{Enter}');
    await waitFor(() => expect(search(history).get('node')).toBe('hangar-2'));
    expect(await screen.findByRole('region', { name: 'Log records of hangar-2' })).toBeInTheDocument();

    await user.click(screen.getByRole('combobox', { name: /^Node/ }));
    await user.click(await screen.findByRole('option', { name: /hangar-3/ }));
    expect(await screen.findByText(/hangar-3" is not running or does not answer/)).toBeInTheDocument();
  });

  it('follows new records, pauses and refreshes on demand', async () => {
    const user = userEvent.setup();
    systemMockState.growth = 0;
    renderApp('/system/logs?level=error');
    await screen.findByRole('region', { name: 'Log records of hangar-1' });
    const controls = screen.getByRole('group', { name: 'Refresh logs' });
    await user.click(within(controls).getByRole('button', { name: 'Pause' }));
    expect(within(controls).getByText('Auto-refresh paused')).toBeInTheDocument();

    systemMockState.log('hangar-1', 'error', 'disk almost full', { path: '/var/lib/hangar' });
    systemMockState.log('hangar-1', 'info', 'not an error');
    await user.click(within(controls).getByRole('button', { name: 'Refresh now' }));
    const view = screen.getByRole('region', { name: 'Log records of hangar-1' });
    expect(await within(view).findByText('disk almost full')).toBeInTheDocument();
    expect(within(view).queryByText('not an error')).not.toBeInTheDocument();
    expect(screen.getByText(/Showing 31 records/)).toBeInTheDocument();
  });

  it('downloads the whole filtered buffer as JSON lines', async () => {
    const user = userEvent.setup();
    let saved: Blob | undefined;
    const create = vi.fn((b: Blob) => {
      saved = b;
      return 'blob:logs';
    });
    Object.assign(URL, { createObjectURL: create, revokeObjectURL: vi.fn() });
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {});
    renderApp('/system/logs?level=warn');
    await screen.findByRole('region', { name: 'Log records of hangar-1' });
    await user.click(screen.getByRole('button', { name: 'Download JSON lines' }));
    await waitFor(() => expect(click).toHaveBeenCalled());
    const lines = (await saved!.text()).trim().split('\n');
    expect(lines.length).toBe(90); // warn and error records of 240
    for (const l of lines) expect(['warn', 'error']).toContain((JSON.parse(l) as { level: string }).level);
  });

  it('needs the cluster admin privilege', async () => {
    withPrivileges(['system:cluster:read']);
    renderApp('/system/logs');
    expect(await screen.findByText(/System logs need the/)).toHaveTextContent('system:cluster:admin');
  });
});

describe('system model', () => {
  const rec = (seq: number, level: LogRecord['level'] = 'info', attrs?: Record<string, unknown>): LogRecord => ({
    seq,
    time: '2026-01-02T03:04:05.678Z',
    level,
    msg: `m${seq}`,
    attrs,
  });

  it('computes virtual rows', () => {
    const heights = [ROW_HEIGHT, rowHeight(rec(2, 'info', { a: 1, b: 2 }), true), ROW_HEIGHT];
    const tops = offsets(heights);
    expect(tops).toEqual([0, ROW_HEIGHT, ROW_HEIGHT + heights[1]!, ROW_HEIGHT * 2 + heights[1]!]);
    expect(visibleRange(tops, 0, ROW_HEIGHT, 0)).toEqual([0, 1]);
    expect(visibleRange(tops, ROW_HEIGHT + 5, 10, 0)).toEqual([1, 2]);
    expect(visibleRange(offsets([]), 0, 100)).toEqual([0, 0]);
    const many = offsets(new Array<number>(5000).fill(ROW_HEIGHT));
    expect(visibleRange(many, 2500 * ROW_HEIGHT, 10 * ROW_HEIGHT, 2)).toEqual([2498, 2512]);
  });

  it('caps appended records and renders inline attributes and JSON lines', () => {
    const kept = appendRecords([rec(1), rec(2)], [rec(3), rec(4)], 3);
    expect(kept.map((r) => r.seq)).toEqual([2, 3, 4]);
    expect(inlineAttrs({ request_id: 'x', path: '/a b', status: 500, error: 'boom' })).toBe('error=boom status=500 path="/a b"');
    expect(toJsonLines([rec(1, 'warn', { k: 'v' })])).toBe('{"time":"2026-01-02T03:04:05.678Z","level":"warn","msg":"m1","k":"v"}\n');
    expect(toJsonLines([])).toBe('');
  });

  it('summarises nodes', () => {
    const node = (id: string, patch: Partial<SystemNode>): SystemNode => ({
      id, hostname: id, url: '', version: '1.0.0', commit: '', started_at: '', uptime_seconds: 1, last_heartbeat: '',
      role: 'follower', status: 'healthy', stale: false, current: false, subsystems: [], ...patch,
    });
    const sub = (status: 'healthy' | 'degraded' | 'down') => ({ name: status, label: status, status, detail: '', checked_at: '' });
    const ok = summarize([node('a', { role: 'leader', subsystems: [sub('healthy'), sub('healthy')] })]);
    expect(ok).toMatchObject({ headline: 'Everything is working', passing: 2, checks: 2, notes: [] });
    const s = summarize([node('a', { status: 'degraded', subsystems: [sub('healthy'), sub('degraded')] })]);
    expect(s).toMatchObject({ status: 'degraded', headline: '1 check needs attention', passing: 1, checks: 2 });
    expect(s.notes[0]).toMatch(/No node holds the scheduler lock/);
    expect(summarize([node('a', { status: 'down', role: 'leader', subsystems: [sub('down'), sub('down'), sub('degraded')] })]).headline).toBe('2 checks are down');
    expect(summarize([]).headline).toBe('Waiting for the first report');
    expect(summarize([node('a', { role: 'leader' }), node('b', { stale: true, status: 'down' })]).headline).toBe('1 node is not responding');
    expect(summarize([node('a', { stale: true, status: 'down' })]).headline).toBe('No node is responding');
    expect(summarize([node('a', { role: 'leader' }), node('b', { stale: true, status: 'down' })]).status).toBe('degraded');
    expect(summarize([node('a', { stale: true, status: 'down' })]).status).toBe('down');
  });

  it('formats times and versions', () => {
    const now = new Date('2026-11-03T12:00:00Z');
    expect(shortTime('2026-11-03T08:52:10Z', now)).toBe('08:52');
    expect(shortTime('2026-11-01T23:05:00Z', now)).toBe('1 Nov 23:05');
    expect(period('2026-11-03T08:52:00Z', '2026-11-03T10:32:00Z', now)).toBe('08:52–10:32');
    expect(period('2026-11-02T23:00:00Z', '2026-11-03T01:00:00Z', now)).toBe('2 Nov 23:00 – 01:00');
    expect(versionText('0.1.1')).toBe('v0.1.1');
    expect(versionText('dev')).toBe('dev');
  });
});
