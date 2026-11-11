import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { DataTable, type Column } from '../components/DataTable';
import { renderApp } from './render';

afterEach(() => {
  window.localStorage.clear();
  vi.restoreAllMocks();
});

describe('sidebar sections', () => {
  it('fold, remember it, and still show the current page', async () => {
    const user = userEvent.setup();
    const first = renderApp('/repositories');
    let nav = await screen.findByRole('navigation', { name: 'Primary' });
    const storage = within(nav).getByRole('button', { name: 'Storage' });
    expect(storage).toHaveAttribute('aria-expanded', 'true');
    expect(within(nav).getByRole('link', { name: 'Blob stores' })).toBeVisible();

    await user.click(storage);
    expect(storage).toHaveAttribute('aria-expanded', 'false');
    expect(within(nav).queryByRole('link', { name: 'Blob stores' })).not.toBeInTheDocument();
    expect(JSON.parse(window.localStorage.getItem('hangar.nav.collapsed') ?? '[]')).toEqual(['Storage']);
    first.unmount();

    // A new visit keeps the section folded...
    const second = renderApp('/repositories');
    nav = await screen.findByRole('navigation', { name: 'Primary' });
    expect(within(nav).getByRole('button', { name: 'Storage' })).toHaveAttribute('aria-expanded', 'false');
    second.unmount();

    // ...but a page inside it opens it, unless it is folded on that page.
    renderApp('/cleanup');
    nav = await screen.findByRole('navigation', { name: 'Primary' });
    await waitFor(() => expect(within(nav).getByRole('link', { name: 'Cleanup policies' })).toHaveAttribute('aria-current', 'page'));
    expect(within(nav).getByRole('button', { name: 'Storage' })).toHaveAttribute('aria-expanded', 'true');
    await user.click(within(nav).getByRole('button', { name: 'Storage' }));
    expect(within(nav).getByRole('button', { name: 'Storage' })).toHaveAttribute('aria-expanded', 'false');
  });

  it('work when the browser blocks storage', async () => {
    const user = userEvent.setup();
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    renderApp('/repositories');
    const nav = await screen.findByRole('navigation', { name: 'Primary' });
    const security = within(nav).getByRole('button', { name: 'Security' });
    await user.click(security);
    expect(security).toHaveAttribute('aria-expanded', 'false');
    expect(within(nav).queryByRole('link', { name: 'Vulnerabilities' })).not.toBeInTheDocument();
  });
});

describe('data table columns', () => {
  interface Row {
    id: string;
    name: string;
    url: string;
  }
  const columns: Column<Row>[] = [
    { key: 'name', header: 'Name', cell: (r) => r.name },
    { key: 'url', header: 'URL', priority: 3, cell: (r) => r.url },
    { key: 'actions', header: <span className="visually-hidden">Actions</span>, cell: () => <button type="button">Copy</button> },
  ];

  it('mark their priority and their label for phone cards', () => {
    render(<DataTable caption="Things" columns={columns} rows={[{ id: '1', name: 'one', url: 'http://example.test/one' }]} rowKey={(r) => r.id} />);
    const table = screen.getByRole('table', { name: 'Things' });
    expect(table).toHaveClass('data-table--stack');
    const [name, url, actions] = within(table).getAllByRole('cell');
    expect(name).toHaveAttribute('data-label', 'Name');
    expect(name).not.toHaveAttribute('data-priority');
    expect(url).toHaveAttribute('data-priority', '3');
    expect(url).toHaveAttribute('data-label', 'URL');
    // The actions column has no visible header, so its cell carries no label.
    expect(actions).toHaveAttribute('data-label', '');
    expect(within(table).getByRole('columnheader', { name: 'URL' })).toHaveAttribute('data-priority', '3');
    expect(screen.getByRole('region', { name: 'Things table' })).toBeInTheDocument();
  });

  it('can keep the table on phones', () => {
    render(<DataTable caption="Things" columns={columns} rows={[]} rowKey={(r) => r.id} stack={false} />);
    expect(screen.getByRole('table', { name: 'Things' })).not.toHaveClass('data-table--stack');
  });
});
