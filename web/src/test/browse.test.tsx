import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { catalogState } from '../mocks/catalog';
import { session } from '../mocks/data';
import { server } from '../mocks/node';
import { renderApp } from './render';

const search = (h: { location: { search: string } }) => new URLSearchParams(h.location.search);

describe('package browser', () => {
  it('lists repositories, then a repository’s packages with its client configuration', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/browse');
    expect(await screen.findByRole('heading', { level: 1, name: 'Packages' })).toBeInTheDocument();
    const tree = screen.getByRole('navigation', { name: 'Repositories and packages' });
    await user.click(await within(tree).findByRole('link', { name: /npm-internal/ }));
    await waitFor(() => expect(search(history).get('repo')).toBe('npm-internal'));
    const pkgs = await within(tree).findByRole('list', { name: 'Packages in npm-internal' });
    expect(within(pkgs).getAllByRole('link').map((a) => a.textContent)).toEqual(['@acme/eslint-config1 versions', '@acme/ui-kit2 versions']);
    expect(await screen.findByRole('heading', { level: 1, name: 'npm-internal' })).toBeInTheDocument();
    expect(screen.getByText('npm config set registry https://pkg.acme.io/repository/npm-internal/')).toBeInTheDocument();

    await user.type(within(tree).getByLabelText('Filter packages'), 'eslint');
    expect(within(pkgs).getAllByRole('link')).toHaveLength(1);
  });

  it('shows a package: header, install commands per client, overview facts', async () => {
    const user = userEvent.setup();
    renderApp('/browse?repo=docker-prod&name=payments-api&version=2.14.0');
    expect(await screen.findByRole('heading', { level: 1, name: 'payments-api' })).toBeInTheDocument();
    const head = screen.getByRole('region', { name: 'payments-api 2.14.0' });
    expect(within(head).getByText('Signed')).toBeInTheDocument();
    expect(within(head).getByText(/linux\/amd64, linux\/arm64 · published by CI · build 8812/)).toBeInTheDocument();
    expect(within(head).getByText('docker pull cr.acme.io/docker-prod/payments-api:2.14.0')).toBeInTheDocument();
    await user.click(await within(head).findByRole('radio', { name: 'Podman' }));
    expect(within(head).getByText('podman pull cr.acme.io/docker-prod/payments-api:2.14.0')).toBeInTheDocument();

    expect(within(head).getByText('186 MB')).toBeInTheDocument();
    expect(within(head).getByText('distroless/java21')).toBeInTheDocument();
    expect(within(head).getByRole('link', { name: 'git.acme.io/payments/commit/7c1e9a2' })).toHaveAttribute('rel', 'noreferrer noopener');
    expect(within(head).getByRole('button', { name: 'Copy digest' })).toBeInTheDocument();
    // The server reports no dependents or findings (501): no tabs for them, nothing invented.
    const tabs = within(head).getByRole('tablist', { name: 'Package details' });
    await waitFor(() => expect(within(tabs).getAllByRole('tab').map((t) => t.textContent)).toEqual(['Overview', 'Versions 3', 'Layers']));
    // The tree expands the package to its versions.
    const versions = await screen.findByRole('list', { name: 'Versions of payments-api' });
    expect(within(versions).getByRole('link', { name: /^2\.14\.0/ })).toHaveAttribute('aria-current', 'page');
  });

  it('switches versions and tabs through the URL, with keyboard tabs', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/browse?repo=docker-prod&name=payments-api');
    await screen.findByRole('heading', { level: 1, name: 'payments-api' });
    await user.selectOptions(screen.getByLabelText('Version'), '2.13.1');
    await waitFor(() => expect(search(history).get('version')).toBe('2.13.1'));
    expect(await screen.findByText('1 high')).toBeInTheDocument();

    screen.getByRole('tab', { name: 'Overview' }).focus();
    await user.keyboard('{ArrowRight}');
    await waitFor(() => expect(search(history).get('tab')).toBe('versions'));
    expect(screen.getByRole('tab', { name: /Versions/ })).toHaveFocus();
    const table = await screen.findByRole('table', { name: 'Versions' });
    expect(await within(table).findByRole('link', { name: '2.14.0' })).toBeInTheDocument();
    expect(within(table).getAllByRole('row')).toHaveLength(4);
  });

  it('lists files with checksums and same-origin download links', async () => {
    renderApp('/browse?repo=npm-internal&ns=@acme&name=ui-kit&tab=files');
    const table = await screen.findByRole('table', { name: 'Files' });
    const link = await within(table).findByRole('link', { name: /ui-kit-5\.3\.1\.tgz/ });
    expect(link).toHaveAttribute('href', '/repository/npm-internal/@acme/ui-kit/-/ui-kit-5.3.1.tgz');
    expect(within(table).getByRole('button', { name: /Copy SHA-256 of/ })).toBeInTheDocument();
  });

  it('deletes a version after confirmation and moves to the newest remaining one', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/browse?repo=docker-prod&name=payments-api&version=2.14.0');
    await screen.findByRole('heading', { level: 1, name: 'payments-api' });
    await user.click(screen.getByRole('button', { name: 'Delete' }));
    const dialog = screen.getByRole('alertdialog', { name: 'Delete payments-api 2.14.0?' });
    expect(within(dialog).getByRole('button', { name: 'Cancel' })).toHaveFocus();
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument();
    expect(catalogState.components.some((c) => c.id === 'c_8812')).toBe(true);

    await user.click(screen.getByRole('button', { name: 'Delete' }));
    await user.click(within(screen.getByRole('alertdialog')).getByRole('button', { name: 'Delete version' }));
    await waitFor(() => expect(catalogState.components.some((c) => c.id === 'c_8812')).toBe(false));
    await waitFor(() => expect(search(history).get('version')).toBeNull());
    expect(await screen.findByRole('region', { name: 'payments-api 2.13.2' })).toBeInTheDocument();
  });

  it('disables deletion without delete rights on the repository', async () => {
    server.use(http.get('*/api/v1/auth/session', () => HttpResponse.json({ ...session, privileges: ['repo:*:read'] })));
    renderApp('/browse?repo=docker-prod&name=payments-api&tab=versions');
    await screen.findByRole('heading', { level: 1, name: 'payments-api' });
    expect(screen.getByRole('button', { name: 'Delete' })).toBeDisabled();
    const table = await screen.findByRole('table', { name: 'Versions' });
    await within(table).findByRole('link', { name: '2.13.2' });
    for (const b of within(table).getAllByRole('button', { name: /Delete version/ })) expect(b).toBeDisabled();
  });

  it('shows dependents and vulnerabilities only when the server reports them', async () => {
    server.use(
      http.get('*/api/v1/components/:id/dependents', () =>
        HttpResponse.json({ items: [{ ...catalogState.components[4]!, id: 'dep1' }], next_cursor: null }),
      ),
      http.get('*/api/v1/components/:id/findings', () =>
        HttpResponse.json({
          items: [{ id: 'f1', advisory_id: 'CVE-2026-0001', title: 'Heap overflow', severity: 'high', package: 'zlib', installed: '1.2.11', fixed_in: '1.2.12', status: 'open', affected_components: 1 }],
          next_cursor: null,
        }),
      ),
    );
    const user = userEvent.setup();
    renderApp('/browse?repo=docker-prod&name=payments-api');
    await user.click(await screen.findByRole('tab', { name: 'Vulnerabilities' }));
    expect(within(screen.getByRole('table', { name: 'Vulnerabilities' })).getByText('CVE-2026-0001')).toBeInTheDocument();
    await user.click(screen.getByRole('tab', { name: 'Dependents' }));
    expect(within(screen.getByRole('table', { name: 'Dependents' })).getByRole('link', { name: '@acme/ui-kit 5.3.1' })).toBeInTheDocument();
  });

  it('says when a version does not exist', async () => {
    renderApp('/browse?repo=docker-prod&name=payments-api&version=9.9.9');
    expect(await screen.findByText(/is not in docker-prod/)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Show the newest version' })).toBeInTheDocument();
  });

  it('resolves the overview’s links, which carry no namespace', async () => {
    renderApp('/browse?repo=npm-internal&name=ui-kit&version=5.3.0');
    expect(await screen.findByRole('region', { name: '@acme/ui-kit 5.3.0' })).toBeInTheDocument();
  });
});
