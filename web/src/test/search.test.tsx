import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { renderApp } from './render';

const q = (h: { location: { search: string } }) => new URLSearchParams(h.location.search).get('q');

describe('search', () => {
  it('explains the query language before the first search', async () => {
    renderApp('/search');
    expect(await screen.findByRole('heading', { level: 1, name: 'Find any package' })).toBeInTheDocument();
    const help = screen.getByRole('region', { name: 'What you can search for' });
    expect(within(help).getByRole('link', { name: 'sha256:9f2c' })).toBeInTheDocument();
    expect(within(help).getByRole('link', { name: 'unused:>90d' })).toBeInTheDocument();
  });

  it('runs the query from the URL, highlights matches and shows facets', async () => {
    renderApp('/search?q=ui');
    const results = await screen.findByRole('list', { name: 'Search results' });
    const hit = await within(results).findByRole('link', { name: /@acme\/ui-kit/ });
    expect(hit).toHaveAttribute('href', '/browse?repo=npm-internal&ns=%40acme&name=ui-kit&version=5.3.1');
    expect(within(hit).getByText('ui', { selector: 'mark' })).toBeInTheDocument();
    expect(hit).toHaveTextContent('npm-internal · 5.3.1 newest · 2 versions · 10.5K downloads');
    expect(screen.getByRole('heading', { level: 2, name: /1 package for “ui”/ })).toBeInTheDocument();
    const filters = screen.getByRole('complementary', { name: 'Filters' });
    expect(within(filters).getByRole('checkbox', { name: /npm/ })).not.toBeChecked();
  });

  it('turns facet clicks into qualifiers in the query and the URL', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/search?q=a');
    const filters = await screen.findByRole('complementary', { name: 'Filters' });
    await user.click(await within(filters).findByRole('checkbox', { name: /Docker/ }));
    await waitFor(() => expect(q(history)).toBe('a format:docker'));
    expect(screen.getByRole('combobox', { name: 'Search query' })).toHaveValue('a format:docker');
    await waitFor(() => expect(within(screen.getByRole('list', { name: 'Search results' })).getAllByRole('listitem')).toHaveLength(2));

    await user.click(within(filters).getByRole('radio', { name: /Has findings/ }));
    await waitFor(() => expect(q(history)).toBe('a format:docker security:findings'));
    await user.selectOptions(within(filters).getByLabelText('Repository'), 'docker-prod');
    await waitFor(() => expect(q(history)).toBe('a format:docker security:findings repo:docker-prod'));
    await user.click(within(filters).getByRole('radio', { name: 'Any' }));
    await waitFor(() => expect(q(history)).toBe('a format:docker repo:docker-prod'));
  });

  it('completes qualifiers with the keyboard and submits', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/search');
    const box = await screen.findByRole('combobox', { name: 'Search query' });
    await user.type(box, 'form');
    expect(await screen.findByRole('option', { name: /format:/ })).toBeInTheDocument();
    await user.keyboard('{Enter}');
    expect(box).toHaveValue('format:');
    await user.keyboard('ma');
    const option = await screen.findByRole('option', { name: /format:maven/ });
    expect(box).toHaveAttribute('aria-activedescendant', option.id);
    await user.keyboard('{Tab}');
    expect(box).toHaveValue('format:maven ');
    await user.keyboard('{Enter}');
    await waitFor(() => expect(q(history)).toBe('format:maven'));
    const results = await screen.findByRole('list', { name: 'Search results' });
    expect(within(results).getAllByRole('listitem')).toHaveLength(2);

    // ArrowDown moves from the box into the results, Escape returns.
    box.focus();
    await user.keyboard('{ArrowDown}');
    const links = within(results).getAllByRole('link');
    expect(links[0]).toHaveFocus();
    await user.keyboard('{ArrowDown}');
    expect(links[1]).toHaveFocus();
    await user.keyboard('{Escape}');
    expect(box).toHaveFocus();
  });

  it('focuses the box with "/"', async () => {
    const user = userEvent.setup();
    renderApp('/search?q=ui');
    await screen.findByRole('list', { name: 'Search results' });
    (document.activeElement as HTMLElement | null)?.blur();
    await user.keyboard('/');
    expect(screen.getByRole('combobox', { name: 'Search query' })).toHaveFocus();
  });

  it('sorts through the URL', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/search?q=a');
    await screen.findByRole('list', { name: 'Search results' });
    await user.selectOptions(screen.getByLabelText('Sort'), '-downloads');
    await waitFor(() => expect(new URLSearchParams(history.location.search).get('sort')).toBe('-downloads'));
    await waitFor(() =>
      expect(within(screen.getByRole('list', { name: 'Search results' })).getAllByRole('listitem')[0]).toHaveTextContent('log4j-core'),
    );
  });

  it('says when nothing matches and offers to drop filters', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/search?q=zzz%20format%3Anpm');
    expect(await screen.findByText('Nothing matches “zzz format:npm”.')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'format:' }));
    await waitFor(() => expect(q(history)).toBe('zzz'));
  });

  it('shows the server’s message for an invalid qualifier', async () => {
    renderApp('/search?q=sha256%3Azz');
    expect(await screen.findByRole('alert')).toHaveTextContent('sha256:zz: want 4 to 64 hex digits');
    expect(screen.getByRole('heading', { level: 2, name: 'Check the query' })).toBeInTheDocument();
  });
});
