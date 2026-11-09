import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';
import type { RepositoryInput, RoutingRules } from '../api/types';
import { mockState } from '../mocks/handlers';
import { server } from '../mocks/node';
import { renderApp } from './render';

const repo = (name: string) => mockState.repositories.find((r) => r.name === name)!;

function captureRouting() {
  const bodies: { method: string; body: RoutingRules | null }[] = [];
  server.events.on('request:start', async ({ request }) => {
    if (new URL(request.url).pathname.endsWith('/routing')) {
      bodies.push({ method: request.method, body: request.method === 'PUT' ? ((await request.clone().json()) as RoutingRules) : null });
    }
  });
  return bodies;
}

afterEach(() => server.events.removeAllListeners());

const panel = async () => screen.findByRole('region', { name: 'Routing rules' });

describe('routing rules', () => {
  it('reserves names for a group member, saves them on their own and tests a name', async () => {
    const calls = captureRouting();
    const user = userEvent.setup();
    renderApp('/repositories/npm-all/settings');
    const p = await panel();
    expect(within(p).getByText(/dependency confusion/)).toBeInTheDocument();
    const save = within(p).getByRole('button', { name: 'Save routing rules' });
    expect(save).toBeDisabled();

    await user.type(within(p).getByLabelText('Names only npm-internal serves'), '@acme/*');
    await user.click(save);
    expect(await within(p).findByRole('button', { name: 'Saved' })).toBeDisabled();
    expect(calls).toEqual([{ method: 'PUT', body: { members: [{ member: 'npm-internal', exclusive: ['@acme/*'], block: [] }] } }]);
    expect(repo('npm-all').config.routing?.members?.[0]?.exclusive).toEqual(['@acme/*']);
    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'Copy repository configuration as YAML' }).parentElement).toHaveTextContent(
        /routing:\s*members:\s*- member: npm-internal\s*exclusive:\s*- "?@acme\/\*"?/,
      ),
    );

    await user.type(within(p).getByLabelText('Test a name'), '@acme/ui{Enter}');
    const result = await within(p).findByRole('status', { name: 'Routing test result' });
    expect(result).toHaveTextContent('Served by npm-internal');
    expect(result).toHaveTextContent('npmjs-proxy excluded: @acme/* is reserved for npm-internal in npm-all');

    // Clearing every pattern deletes the rules.
    await user.clear(within(p).getByLabelText('Names only npm-internal serves'));
    await user.click(within(p).getByRole('button', { name: 'Save routing rules' }));
    await waitFor(() => expect(repo('npm-all').config.routing).toBeUndefined());
    expect(calls.at(-1)?.method).toBe('DELETE');
  });

  it('suggests patterns from what hosted members publish', async () => {
    const user = userEvent.setup();
    renderApp('/repositories/npm-all/settings');
    const p = await panel();
    await user.click(within(p).getByRole('button', { name: 'Block names your hosted repositories publish' }));
    expect(await within(p).findByText('Added @acme/*. Review and save.')).toBeInTheDocument();
    expect(within(p).getByLabelText('Names only npm-internal serves')).toHaveValue('@acme/*');
    expect(within(p).getByRole('button', { name: 'Save routing rules' })).toBeEnabled();
    await user.click(within(p).getByRole('button', { name: 'Discard' }));
    expect(within(p).getByLabelText('Names only npm-internal serves')).toHaveValue('');
  });

  it('blocks names on a proxy and keeps the rules when other settings are saved', async () => {
    const puts: RepositoryInput[] = [];
    server.events.on('request:start', async ({ request }) => {
      if (request.method === 'PUT' && new URL(request.url).pathname.endsWith('/repositories/nuget-proxy')) {
        puts.push((await request.clone().json()) as RepositoryInput);
      }
    });
    const user = userEvent.setup();
    renderApp('/repositories/nuget-proxy/settings');
    const p = await panel();
    expect(within(p).queryByRole('table')).not.toBeInTheDocument();
    await user.type(within(p).getByLabelText('Block'), 'Edcom.*, Acme.*{Enter}');
    expect(await within(p).findByRole('button', { name: 'Saved' })).toBeInTheDocument();
    expect(repo('nuget-proxy').config.routing).toEqual({ allow: [], block: ['Edcom.*', 'Acme.*'] });

    await user.type(within(p).getByLabelText('Test a name'), 'edcom.common.utils');
    await user.click(within(p).getByRole('button', { name: 'Test' }));
    expect(await within(p).findByRole('status', { name: 'Routing test result' })).toHaveTextContent('Blocked: nuget-proxy blocks Edcom.*');

    const form = screen.getByRole('form', { name: 'Settings of nuget-proxy' });
    await user.type(within(form).getByLabelText(/Description/), 'nuget.org');
    await user.click(screen.getByRole('button', { name: 'Save changes' }));
    await waitFor(() => expect(puts).toHaveLength(1));
    expect(puts[0]!.config.routing?.block).toEqual(['Edcom.*', 'Acme.*']);
  });

  it('shows the server error and no panel for hosted repositories', async () => {
    server.use(
      http.put('*/api/v1/repositories/:name/routing', () =>
        HttpResponse.json({ code: 'invalid_argument', message: 'routing block: routing pattern "[" is invalid' }, { status: 400 }),
      ),
    );
    const user = userEvent.setup();
    const { unmount } = renderApp('/repositories/nuget-proxy/settings');
    const p = await panel();
    await user.type(within(p).getByLabelText('Block'), '[[');
    await user.click(within(p).getByRole('button', { name: 'Save routing rules' }));
    expect(await within(p).findByRole('alert')).toHaveTextContent('routing pattern "[" is invalid');
    unmount();

    renderApp('/repositories/npm-internal/settings');
    await screen.findByRole('form', { name: 'Settings of npm-internal' });
    expect(screen.queryByRole('region', { name: 'Routing rules' })).not.toBeInTheDocument();
  });
});
