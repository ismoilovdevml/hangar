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
  summarize,
  toJsonLines,
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
  it('shows every node with its role, version and subsystems', async () => {
    renderApp('/system/status');
    const summary = await screen.findByRole('region', { name: 'Summary' });
    expect(within(summary).getByText('Degraded')).toBeInTheDocument();
    expect(within(summary).getByText('1 of 3 nodes are not responding.')).toBeInTheDocument();
    expect(within(summary).getByText('2 of 3')).toBeInTheDocument();
    // The silent node's older version does not count.
    expect(within(summary).getByText('Version')).toBeInTheDocument();
    expect(within(summary).getByText('1.0.0')).toBeInTheDocument();

    const one = screen.getByRole('region', { name: /^hangar-1/ });
    expect(within(one).getByText('This node')).toBeInTheDocument();
    expect(within(one).getByText('Scheduler leader')).toBeInTheDocument();
    expect(within(one).getByText('http://10.0.1.11:8080')).toBeInTheDocument();
    const subsystems = within(one).getByRole('list', { name: 'Subsystems of hangar-1' });
    expect(within(subsystems).getAllByRole('listitem').length).toBe(7);
    const db = within(subsystems).getByRole('listitem', { name: 'PostgreSQL: Healthy' });
    expect(within(db).getByText('Ping 2 ms; 6 of 40 connections in use')).toBeInTheDocument();
    expect(within(db).getByText('PostgreSQL 17.2')).toBeInTheDocument();
    expect(db.querySelector('.subsystem__meta')).toHaveTextContent(/^Checked \d+ s ago · 2 ms$/);

    const two = screen.getByRole('region', { name: /^hangar-2/ });
    expect(within(two).getByRole('listitem', { name: 'Upstream proxies: Degraded' })).toHaveTextContent('maven-central');

    const three = screen.getByRole('region', { name: /^hangar-3/ });
    expect(within(three).getByRole('note')).toHaveTextContent(/No heartbeat for 30 min/);
  });

  it('refreshes on demand and can pause auto-refresh', async () => {
    const user = userEvent.setup();
    renderApp('/system/status');
    await screen.findByRole('region', { name: 'Summary' });
    const controls = screen.getByRole('group', { name: 'Refresh status' });
    const before = systemMockState.nodesRequests;
    await user.click(within(controls).getByRole('button', { name: 'Refresh now' }));
    await waitFor(() => expect(systemMockState.nodesRequests).toBe(before + 1));
    await waitFor(() => expect(within(controls).getByText(/Refreshing in \d+ s/)).toBeInTheDocument());

    await user.click(within(controls).getByRole('button', { name: 'Pause' }));
    expect(within(controls).getByText('Auto-refresh paused')).toBeInTheDocument();
    await user.click(within(controls).getByRole('button', { name: 'Resume' }));
    expect(within(controls).queryByText('Auto-refresh paused')).not.toBeInTheDocument();
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
    expect(summarize([node('a', { role: 'leader' })]).message).toBe('All nodes and subsystems are healthy.');
    const s = summarize([node('a', { status: 'degraded' })]);
    expect(s.status).toBe('degraded');
    expect(s.message).toMatch(/No node holds the scheduler lock/);
    expect(summarize([]).message).toBe('No node has reported yet.');
    expect(summarize([node('a', { role: 'leader' }), node('b', { stale: true, status: 'down' })]).status).toBe('degraded');
    expect(summarize([node('a', { stale: true, status: 'down' })]).status).toBe('down');
  });
});
