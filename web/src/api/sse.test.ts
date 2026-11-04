import { QueryClient } from '@tanstack/react-query';
import { describe, expect, it, vi } from 'vitest';
import { applyLiveEvent } from './events';
import { queryKeys } from './hooks';
import { createSseParser, openEventStream, type SseMessage, type StreamStatus } from './sse';
import type { Page, PublishedComponent, UpstreamHealth } from './types';
import { recentComponents, upstreams } from '../mocks/data';

describe('SSE parser', () => {
  it('handles chunks split anywhere, CRLF, comments, multi-line data and sticky ids', () => {
    const got: SseMessage[] = [];
    const p = createSseParser((m) => got.push(m));
    const stream = ': ping\r\nretry: 2500\r\n\r\nid: 41\r\nevent: component.published\r\ndata: {"a":\r\ndata: 1}\r\n\r\nevent: upstream.status\ndata: {}\n\n';
    for (let i = 0; i < stream.length; i += 3) p.push(stream.slice(i, i + 3));
    expect(got).toEqual([
      { id: '41', event: 'component.published', data: '{"a":\n1}' },
      { id: '41', event: 'upstream.status', data: '{}' },
    ]);
    expect(p.retry).toBe(2500);
    expect(p.lastEventId).toBe('41');
  });

  it('does not dispatch a message without data', () => {
    const got: SseMessage[] = [];
    const p = createSseParser((m) => got.push(m));
    p.push('event: x\n\nid: 9\n\n');
    expect(got).toEqual([]);
    expect(p.lastEventId).toBe('9');
  });
});

function streamResponse(text: string, status = 200): Response {
  const body = new ReadableStream<Uint8Array>({
    start(c) {
      c.enqueue(new TextEncoder().encode(text));
      c.close();
    },
  });
  return new Response(body, { status, headers: { 'Content-Type': 'text/event-stream' } });
}

describe('openEventStream', () => {
  it('reconnects after the stream ends and resumes with Last-Event-ID', async () => {
    const calls: (string | null)[] = [];
    const responses = [
      () => streamResponse('id: 7\nevent: component.published\ndata: {"n":1}\n\n'),
      () => new Response('{"code":"unavailable"}', { status: 503 }),
      () => streamResponse('id: 8\nevent: component.published\ndata: {"n":2}\n\n'),
    ];
    const fetchImpl = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => {
      calls.push(new Headers(init?.headers).get('Last-Event-ID'));
      const next = responses.shift();
      return next ? next() : new Promise<Response>(() => {});
    });
    const messages: string[] = [];
    const statuses: StreamStatus[] = [];
    const close = openEventStream({
      url: '/api/v1/events',
      fetchImpl: fetchImpl as unknown as typeof fetch,
      retryMs: 1,
      onMessage: (m) => messages.push(m.data),
      onStatus: (s) => statuses.push(s),
    });
    await vi.waitFor(() => expect(fetchImpl).toHaveBeenCalledTimes(4));
    close();
    expect(messages).toEqual(['{"n":1}', '{"n":2}']);
    expect(calls).toEqual([null, '7', '7', '8']);
    expect(statuses).toContain('reconnecting');
  });

  it('stops and reports a 401 instead of retrying', async () => {
    const fetchImpl = vi.fn(async () => new Response('{}', { status: 401 }));
    const onUnauthorized = vi.fn();
    const statuses: StreamStatus[] = [];
    openEventStream({
      url: '/api/v1/events',
      fetchImpl: fetchImpl as unknown as typeof fetch,
      retryMs: 1,
      onMessage: () => {},
      onStatus: (s) => statuses.push(s),
      onUnauthorized,
    });
    await vi.waitFor(() => expect(onUnauthorized).toHaveBeenCalledOnce());
    await new Promise((r) => setTimeout(r, 20));
    expect(fetchImpl).toHaveBeenCalledOnce();
    expect(statuses.at(-1)).toBe('closed');
  });
});

describe('applyLiveEvent', () => {
  it('prepends published components, marks quarantines, drops deletions and updates upstreams', () => {
    const qc = new QueryClient();
    const recent = recentComponents();
    qc.setQueryData<Page<PublishedComponent>>(queryKeys.recentComponents, { items: recent, next_cursor: null });
    qc.setQueryData<Page<UpstreamHealth>>(queryKeys.upstreamHealth('today'), { items: upstreams, next_cursor: null });
    const invalidated: unknown[] = [];
    const inv = (k: readonly unknown[]) => invalidated.push(k);

    const fresh = { ...recent[0]!, id: 'c_new', name: 'fresh' };
    applyLiveEvent(qc, { type: 'component.published', data: fresh }, inv);
    applyLiveEvent(qc, { type: 'component.quarantined', data: { id: recent[1]!.id, name: 'x', version: '1' } }, inv);
    applyLiveEvent(qc, { type: 'component.deleted', data: { id: recent[2]!.id } }, inv);
    const npm = upstreams[1]!;
    applyLiveEvent(qc, { type: 'upstream.status', data: { name: npm.name, url: npm.url, state: 'healthy', previous: 'slow' } }, inv);

    const items = qc.getQueryData<Page<PublishedComponent>>(queryKeys.recentComponents)!.items;
    expect(items[0]!.id).toBe('c_new');
    expect(items.find((c) => c.id === recent[1]!.id)!.scan.state).toBe('quarantined');
    expect(items.some((c) => c.id === recent[2]!.id)).toBe(false);
    const up = qc.getQueryData<Page<UpstreamHealth>>(queryKeys.upstreamHealth('today'))!.items;
    // The new state is applied; the recorded latency stays.
    expect(up.find((u) => u.name === 'npmjs-proxy')).toMatchObject({ state: 'healthy', latency_ms: npm.latency_ms });
    expect(up).toHaveLength(upstreams.length);
    expect(invalidated).toContainEqual(queryKeys.upstreams);
    expect(invalidated).toContainEqual(['metrics', 'overview']);
  });
});
