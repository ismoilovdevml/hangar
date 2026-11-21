import { choose } from './choose';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { server } from '../mocks/node';
import { renderApp } from './render';

const q = (h: { location: { search: string } }) => new URLSearchParams(h.location.search).get('q');

describe('search', () => {
  it('explains the query language before the first search', async () => {
    renderApp('/search');
    expect(await screen.findByRole('heading', { level: 1, name: 'Find any package' })).toBeInTheDocument();
    const help = screen.getByRole('region', { name: 'What you can search for' });
    expect(within(help).getByRole('link', { name: 'sha256:9f2c' })).toBeInTheDocument();
    expect(within(help).getByRole('link', { name: 'unused:>90d' })).toBeInTheDocument();
    // Examples name a repository the user has, never one that does not exist.
    expect(await within(help).findByRole('link', { name: 'repo:docker-prod' })).toBeInTheDocument();
    expect(screen.queryByText('repo:npm-hosted')).not.toBeInTheDocument();
    const tryRow = screen.getByText('Try:').parentElement!;
    expect(within(tryRow).getByRole('link', { name: 'repo:docker-prod' })).toBeInTheDocument();
    // One search box on this page: the top bar leaves its search out.
    expect(screen.getAllByRole('combobox', { name: 'Search query' })).toHaveLength(1);
    expect(screen.queryByRole('button', { name: /Search packages, digests, CVEs/ })).not.toBeInTheDocument();
  });

  it('shows the repository qualifier as syntax when there are no repositories', async () => {
    server.use(http.get('*/api/v1/repositories', () => HttpResponse.json({ items: [], next_cursor: null, counts: { all: 0, hosted: 0, proxy: 0, group: 0 } })));
    renderApp('/search');
    const help = await screen.findByRole('region', { name: 'What you can search for' });
    await waitFor(() => expect(within(help).getByText('repo:<name>')).toBeInTheDocument());
    expect(within(help).queryByRole('link', { name: /^repo:/ })).not.toBeInTheDocument();
    expect(within(screen.getByText('Try:').parentElement!).queryByRole('link', { name: /^repo:/ })).not.toBeInTheDocument();
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
    await choose(user, within(filters).getByLabelText('Repository'), 'docker-prod');
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
    // Nothing is picked until the arrows pick it.
    expect(box).not.toHaveAttribute('aria-activedescendant');
    await user.keyboard('{ArrowDown}{Enter}');
    expect(box).toHaveValue('format:');
    await user.keyboard('ma');
    const option = await screen.findByRole('option', { name: /format:maven/ });
    await user.keyboard('{ArrowDown}');
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

  it('searches on Enter while suggestions are open but none is picked', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/search');
    const box = await screen.findByRole('combobox', { name: 'Search query' });
    await user.type(box, 'security');
    expect(await screen.findByRole('option', { name: /security:/ })).toBeInTheDocument();
    await user.keyboard('{Enter}');
    await waitFor(() => expect(q(history)).toBe('security'));
    expect(box).toHaveValue('security');
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
    await choose(user, screen.getByLabelText('Sort'), '-downloads');
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

  it('lists every problem of an invalid query in words, under the box', async () => {
    renderApp('/search?q=format%3A%20sha256%3Azz%20published%3A%3Cnotadate%20unused%3A%3Exyz');
    const problems = await screen.findByRole('alert');
    expect(within(problems).getAllByRole('listitem').map((li) => li.textContent)).toEqual([
      'format: format: needs a value, e.g. format:npm.',
      'sha256:zz sha256: takes 4 to 64 hex digits (0-9, a-f), e.g. sha256:9f2c.',
      'published:<notadate published: takes a date, optionally with < or >, e.g. published:<2026-01-01.',
      'unused:>xyz unused: takes a number of hours, days or weeks, e.g. unused:>90d.',
    ]);
    expect(problems).not.toHaveTextContent('invalid argument');
    // No results column and no Sort for a query that cannot run.
    expect(screen.queryByRole('combobox', { name: 'Sort' })).not.toBeInTheDocument();
  });

  it('warns about an unknown qualifier and an unknown format', async () => {
    renderApp('/search?q=fromat%3Anpm%20format%3Afoo%20lodash');
    const problems = await screen.findByRole('list', { name: 'Problems in the query' });
    expect(problems).toHaveTextContent('fromat: is not a search qualifier, so “fromat:npm” is searched as text. Did you mean format:?');
    expect(await within(problems).findByText(/foo is not a format of this server/)).toBeInTheDocument();
  });
});
