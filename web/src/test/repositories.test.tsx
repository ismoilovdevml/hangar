import { choose, valueOf } from './choose';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { formatBytes } from '../lib/format';
import { repositories } from '../mocks/data';
import { renderApp } from './render';

const all = repositories();
const count = (t: string) => all.filter((r) => r.type === t).length;
const search = (h: { location: { search: string } }) => new URLSearchParams(h.location.search);

describe('repositories list', () => {
  it('shows count cards, tab counts and repository rows from the API', async () => {
    renderApp('/repositories');
    const table = await screen.findByRole('table', { name: 'Repositories' });
    expect(await within(table).findByText('docker-prod')).toBeInTheDocument();

    const cards = screen.getByRole('region', { name: 'Repository counts' });
    expect(within(cards).getByRole('link', { name: `${count('hosted')} Hosted · your own packages` })).toBeInTheDocument();
    expect(within(cards).getByRole('link', { name: `${count('proxy')} Proxy · cached upstreams` })).toBeInTheDocument();
    expect(within(cards).getByRole('link', { name: `${count('group')} Group · one URL for many` })).toBeInTheDocument();

    const tabs = screen.getByRole('navigation', { name: 'Repository type' });
    expect(within(tabs).getByRole('link', { name: `All ${all.length}` })).toHaveAttribute('aria-current', 'page');

    const row = within(table).getByText('npmjs-proxy').closest('tr')!;
    expect(within(row).getByText('Proxy')).toBeInTheDocument();
    expect(within(row).getByText('registry.npmjs.org')).toBeInTheDocument();
    expect(within(row).getByText('Slow upstream')).toBeInTheDocument();
    expect(within(row).getByText('435 GB')).toBeInTheDocument();
    expect(within(row).getByRole('button', { name: 'Copy URL of npmjs-proxy' })).toBeInTheDocument();
    expect(within(within(table).getByText('ghcr-proxy').closest('tr')!).getByText('Authentication failed (401)')).toHaveAttribute(
      'title',
      'Check the proxy credentials.',
    );
    // A group stores nothing itself: it shows what it serves through its members.
    const dockerAll = all.find((r) => r.name === 'docker-all')!;
    const groupRow = within(table).getByText('docker-all').closest('tr')!;
    expect(dockerAll.size_bytes).toBeGreaterThan(0);
    expect(within(groupRow).getByText('via 3 members')).toBeInTheDocument();
    expect(within(groupRow).getByText(`${dockerAll.component_count} versions`)).toBeInTheDocument();
    expect(within(groupRow).getByTitle(/blobs they share are counted once/)).toHaveTextContent(
      `${formatBytes(dockerAll.size_bytes)} via 3 members`,
    );
    expect(within(groupRow).queryByText('—')).not.toBeInTheDocument();
    expect(screen.getByText(`Showing 20 of ${all.length}`)).toBeInTheDocument();
  });

  it('shows last activity, with the exact time on hover, and never-used repositories', async () => {
    renderApp('/repositories?q=npm');
    const table = await screen.findByRole('table', { name: 'Repositories' });
    const proxyRow = (await within(table).findByText('npmjs-proxy')).closest('tr')!;
    const when = within(proxyRow).getByText('2 min ago');
    expect(when.tagName).toBe('TIME');
    expect(when).toHaveAttribute('title', expect.stringMatching(/ UTC$/));
    expect(within(within(table).getByText('npm-sandbox').closest('tr')!).getByText('No activity yet')).toBeInTheDocument();
    // A group is as recent as its busiest member.
    expect(within(within(table).getByText('npm-all').closest('tr')!).getByText('2 min ago')).toBeInTheDocument();
  });

  it('sorts by a column header through the URL, before paging', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/repositories');
    const table = await screen.findByRole('table', { name: 'Repositories' });
    await within(table).findByText('docker-all');
    expect(within(table).getByRole('columnheader', { name: /Name/ })).toHaveAttribute('aria-sort', 'ascending');

    await user.click(within(table).getByRole('button', { name: 'Last activity' }));
    await waitFor(() => expect(search(history).get('sort')).toBe('-last_activity'));
    expect(within(table).getByRole('columnheader', { name: /Last activity/ })).toHaveAttribute('aria-sort', 'descending');
    // Most recent first; the group ties with its busiest member and the name breaks the tie.
    await waitFor(() => expect(within(table).getAllByRole('row')[2]).toHaveTextContent('dockerhub-proxy'));
    expect(within(table).getAllByRole('row')[1]).toHaveTextContent('docker-all');

    await user.click(within(table).getByRole('button', { name: 'Name' }));
    await waitFor(() => expect(search(history).get('sort')).toBeNull());
    await user.click(within(table).getByRole('button', { name: 'Name' }));
    await waitFor(() => expect(search(history).get('sort')).toBe('-name'));
    await waitFor(() => expect(within(table).getAllByRole('row')[1]).toHaveTextContent('raw-legacy'));
  });

  it('keeps the type tab, format and name filters in the URL', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/repositories');
    const tabs = await screen.findByRole('navigation', { name: 'Repository type' });

    await user.click(within(tabs).getByRole('link', { name: /^Proxy/ }));
    await waitFor(() => expect(search(history).get('type')).toBe('proxy'));
    const table = screen.getByRole('table', { name: 'Repositories' });
    await waitFor(() => expect(within(table).queryByText('docker-prod')).not.toBeInTheDocument());
    expect(within(table).getByText('dockerhub-proxy')).toBeInTheDocument();
    expect(within(tabs).getByRole('link', { name: /^Proxy/ })).toHaveAttribute('aria-current', 'page');

    await choose(user, screen.getByLabelText('Format'), 'npm');
    await waitFor(() => expect(search(history).get('format')).toBe('npm'));
    expect(search(history).get('type')).toBe('proxy');
    await waitFor(() => expect(within(table).queryByText('dockerhub-proxy')).not.toBeInTheDocument());
    expect(within(table).getByText('npmjs-proxy')).toBeInTheDocument();
    // Tab counts follow the format filter.
    expect(within(tabs).getByRole('link', { name: `All ${all.filter((r) => r.format === 'npm').length}` })).toBeInTheDocument();

    await user.click(within(tabs).getByRole('link', { name: /^All/ }));
    await waitFor(() => expect(search(history).get('type')).toBeNull());
    await user.type(screen.getByLabelText('Filter by name'), 'internal');
    await waitFor(() => expect(search(history).get('q')).toBe('internal'));
    await waitFor(() => expect(within(table).queryByText('npm-all')).not.toBeInTheDocument());
    expect(within(table).getByText('npm-internal')).toBeInTheDocument();
  });

  it('restores filters from a shared link', async () => {
    renderApp('/repositories?type=group&format=docker');
    const table = await screen.findByRole('table', { name: 'Repositories' });
    expect(await within(table).findByText('docker-all')).toBeInTheDocument();
    expect(within(table).getAllByRole('row')).toHaveLength(2); // header + one group
    expect(valueOf(screen.getByLabelText('Format'))).toBe('docker');
    expect(within(screen.getByRole('navigation', { name: 'Repository type' })).getByRole('link', { name: /^Group/ })).toHaveAttribute(
      'aria-current',
      'page',
    );
  });

  it('pages with the server cursor and comes back with Previous', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/repositories');
    const table = await screen.findByRole('table', { name: 'Repositories' });
    await within(table).findByText('docker-prod');
    expect(screen.getByRole('button', { name: 'Previous' })).toBeDisabled();

    await user.click(screen.getByRole('button', { name: 'Next' }));
    await waitFor(() => expect(search(history).get('cursor')).toBe('o20'));
    expect(await within(table).findByText(all[20]!.name)).toBeInTheDocument();
    expect(screen.getByText(`Showing ${all.length - 20} of ${all.length}`)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Next' })).toBeDisabled();

    await user.click(screen.getByRole('button', { name: 'Previous' }));
    await waitFor(() => expect(search(history).get('cursor')).toBeNull());
    expect(await within(table).findByText('docker-prod')).toBeInTheDocument();
  });

  it('says when nothing matches and offers a way back', async () => {
    renderApp('/repositories?q=does-not-exist');
    expect(await screen.findByText(/No repositories match these filters/)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Show all' })).toBeInTheDocument();
  });
});
