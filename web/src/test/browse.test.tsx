import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { catalogState } from '../mocks/catalog';
import { session } from '../mocks/data';
import { server } from '../mocks/node';
import { renderApp } from './render';

const search = (h: { location: { search: string } }) => new URLSearchParams(h.location.search);
const tree = () => screen.getByRole('tree', { name: 'Repositories' });
const item = async (name: RegExp | string) => within(await screen.findByRole('tree', { name: 'Repositories' })).findByRole('treeitem', { name });
const activeItem = () => document.getElementById(tree().getAttribute('aria-activedescendant') ?? '');
const activeName = () => activeItem()?.querySelector('.tree-row__name')?.textContent;

describe('package browser: tree', () => {
  it('lists repositories with format and type, and loads a repository’s content when expanded', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/browse');
    expect(await screen.findByRole('heading', { level: 1, name: 'Packages' })).toBeInTheDocument();
    const npm = await item(/^npm-internal/);
    expect(npm).toHaveTextContent('Hosted');
    expect(npm).toHaveAttribute('aria-expanded', 'false');
    expect(npm).toHaveAttribute('aria-level', '1');
    expect(within(npm).getByText('npm')).toBeInTheDocument(); // the format icon's label

    await user.click(npm);
    await waitFor(() => expect(search(history).get('repo')).toBe('npm-internal'));
    expect(await screen.findByRole('heading', { level: 1, name: 'npm-internal' })).toBeInTheDocument();
    expect(npm).toHaveAttribute('aria-expanded', 'true');
    // npm: scope, then packages, then the package's files.
    const scope = await item(/^@acme/);
    expect(scope).toHaveAttribute('aria-level', '2');
    await user.click(scope);
    await waitFor(() => expect(search(history).get('path')).toBe('@acme/'));
    expect(await screen.findByRole('table', { name: 'Contents of @acme/' })).toBeInTheDocument();
    await user.click(await item(/^ui-kit\s*package/));
    await waitFor(() => expect(search(history).get('name')).toBe('ui-kit'));
    expect(search(history).get('path')).toBe('@acme/ui-kit/');
    expect(await screen.findByRole('heading', { level: 1, name: '@acme/ui-kit' })).toBeInTheDocument();
    expect(await item(/^ui-kit-5\.3\.1\.tgz/)).toHaveAttribute('aria-level', '4');
    expect(await item(/^meta/)).toBeInTheDocument();
    expect(document.querySelector('select')).toBeNull();
  });

  it('is keyboard navigable', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/browse');
    await item(/^docker-all/);
    tree().focus();
    expect(activeName()).toBe('docker-all');
    await user.keyboard('{ArrowDown}');
    expect(activeName()).toBe('docker-dev');
    await user.keyboard('{ArrowDown}');
    const prod = activeItem()!;
    expect(activeName()).toBe('docker-prod');
    await user.keyboard('{ArrowRight}');
    await waitFor(() => expect(prod).toHaveAttribute('aria-expanded', 'true'));
    await item(/^payments-api/);
    await user.keyboard('{ArrowRight}');
    expect(activeName()).toBe('auth-gateway');
    await user.keyboard('{ArrowDown}');
    expect(activeName()).toBe('payments-api');
    await user.keyboard('{Enter}');
    await waitFor(() => expect(search(history).get('name')).toBe('payments-api'));
    expect(search(history).get('path')).toBe('payments-api/');
    expect(activeItem()).toHaveAttribute('aria-selected', 'true');
    expect(activeItem()).toHaveAttribute('aria-expanded', 'true');
    await user.keyboard('{ArrowLeft}'); // collapse
    expect(activeItem()).toHaveAttribute('aria-expanded', 'false');
    await user.keyboard('{ArrowLeft}'); // to the parent
    expect(activeName()).toBe('docker-prod');
    await user.keyboard('{ArrowLeft}');
    expect(activeItem()).toHaveAttribute('aria-expanded', 'false');
    await user.keyboard('{End}');
    expect(activeName()).toBe('raw-legacy');
    await user.keyboard('{Home}');
    expect(activeName()).toBe('docker-all');
  });

  it('opens the folders of a deep link and selects the node', async () => {
    renderApp('/browse?repo=maven-releases&path=com/acme/ledger-core/7.0.0/&ns=com.acme&name=ledger-core&version=7.0.0');
    const version = await item(/^7\.0\.0\s*version/);
    expect(version).toHaveAttribute('aria-selected', 'true');
    expect(version).toHaveAttribute('aria-level', '5');
    expect(await item(/^ledger-core-7\.0\.0\.jar/)).toBeInTheDocument();
    expect(await item(/^maven-metadata\.xml/)).toBeInTheDocument();
    expect(await screen.findByRole('heading', { level: 1, name: 'com.acme:ledger-core' })).toBeInTheDocument();
  });

  it('filters repositories and loaded folders', async () => {
    const user = userEvent.setup();
    renderApp('/browse?repo=npm-internal&path=@acme/');
    await item(/^ui-kit/);
    await user.type(screen.getByLabelText('Filter the tree'), 'eslint');
    await waitFor(() => expect(within(tree()).getAllByRole('treeitem').map((r) => r.querySelector('.tree-row__name')?.textContent)).toEqual(['npm-internal', '@acme', 'eslint-config']));
    await user.clear(screen.getByLabelText('Filter the tree'));
    await user.type(screen.getByLabelText('Filter the tree'), 'nothing-like-this');
    expect(await screen.findByText(/Nothing loaded matches/)).toBeInTheDocument();
  });

  it('merges a group’s members and names the member that holds each entry', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/browse?repo=docker-all&path=payments-api/');
    expect(await item(/^payments-api/)).toBeInTheDocument();
    await user.click(await item(/^manifests/));
    await user.click(await item(/^2\.14\.0/));
    await waitFor(() => expect(search(history).get('asset')).toMatch(/^c_8812_m_/));
    const head = await screen.findByRole('region', { name: 'File 2.14.0' });
    expect(within(head).getByText(/served through docker-all/)).toBeInTheDocument();
    expect(within(head).getByRole('link', { name: 'docker-prod' })).toBeInTheDocument();
  });

  it('shows the repository summary: members of a group, upstream of a proxy', async () => {
    renderApp('/browse?repo=npm-all');
    const members = await screen.findByRole('region', { name: 'Members' });
    expect(within(members).getByRole('link', { name: 'npm-internal' })).toBeInTheDocument();
    expect(within(members).getByText('Receives publishes')).toBeInTheDocument();
  });

  it('shows the upstream of a proxy', async () => {
    renderApp('/browse?repo=maven-central');
    expect(await screen.findByText('https://repo1.maven.org/maven2/')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Copy upstream URL of maven-central' })).toBeInTheDocument();
  });
});

describe('package browser: package', () => {
  it('shows a package: header, version card, set-me-up clients and a safely rendered README', async () => {
    const user = userEvent.setup();
    renderApp('/browse?repo=npm-internal&ns=@acme&name=ui-kit');
    const head = await screen.findByRole('region', { name: '@acme/ui-kit 5.3.1' });
    expect(within(head).getByText('Newest')).toBeInTheDocument();
    expect(await within(head).findByRole('link', { name: /Download/ })).toHaveAttribute('href', '/repository/npm-internal/@acme/ui-kit/-/ui-kit-5.3.1.tgz');

    const card = screen.getByRole('region', { name: 'Version' });
    expect(await within(card).findByText('MIT')).toBeInTheDocument();
    expect(within(card).getByText(/8\.1K this version/)).toBeInTheDocument();
    expect(within(card).getByText(/10\.5K in total/)).toBeInTheDocument();
    expect(within(card).getByText('No known vulnerabilities')).toBeInTheDocument();
    expect(within(card).getByText('react')).toBeInTheDocument();

    const setup = screen.getByRole('region', { name: 'Set me up' });
    expect(within(setup).getByText('npm install @acme/ui-kit@5.3.1 --registry https://pkg.acme.io/repository/npm-internal/')).toBeInTheDocument();
    await user.click(within(setup).getByRole('radio', { name: 'pnpm' }));
    expect(within(setup).getByText('pnpm add @acme/ui-kit@5.3.1 --registry https://pkg.acme.io/repository/npm-internal/')).toBeInTheDocument();

    const readme = await screen.findByRole('article', { name: 'README' });
    expect(within(readme).getByRole('heading', { name: 'Acme UI kit' })).toBeInTheDocument();
    expect(within(readme).getByRole('link', { name: 'the design system' })).toHaveAttribute('rel', 'noopener noreferrer nofollow');
    expect(within(readme).getByRole('table')).toBeInTheDocument();
    expect(within(readme).getByText('npm install @acme/ui-kit')).toBeInTheDocument();
    expect(within(readme).queryByRole('link', { name: 'Unsafe link' })).toBeNull();
    expect(within(readme).getByText('Unsafe link')).toBeInTheDocument();
    expect(within(readme).getByText('[image: blocked image]')).toBeInTheDocument();
    expect(readme.querySelector('script')).toBeNull();
    expect(readme.textContent).not.toContain('alert');
  });

  it('lists versions with the newest marked, sizes, and switches through the URL', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/browse?repo=docker-prod&name=payments-api');
    await screen.findByRole('heading', { level: 1, name: 'payments-api' });
    await user.click(screen.getByRole('button', { name: '3 versions' }));
    await waitFor(() => expect(search(history).get('tab')).toBe('versions'));
    const table = await screen.findByRole('table', { name: 'Versions' });
    await within(table).findByRole('link', { name: '2.13.1' });
    expect(within(table).getAllByRole('row')).toHaveLength(4);
    expect(within(table).getAllByText('Newest')).toHaveLength(1);
    expect(within(table).getByText('186 MB')).toBeInTheDocument();
    await user.click(within(table).getByRole('link', { name: '2.13.1' }));
    await waitFor(() => expect(search(history).get('version')).toBe('2.13.1'));
    expect(await screen.findByRole('region', { name: 'payments-api 2.13.1' })).toBeInTheDocument();
    expect(screen.getByRole('region', { name: 'Version' })).toHaveTextContent('0 critical · 1 high · 0 medium · 2 low');

    screen.getByRole('tab', { name: /Versions/ }).focus();
    await user.keyboard('{ArrowLeft}');
    await waitFor(() => expect(search(history).get('tab')).toBeNull());
    expect(screen.getByRole('tab', { name: 'Readme' })).toHaveFocus();
    expect(await screen.findByText('No README was published with this version.')).toBeInTheDocument();
  });

  it('lists files with checksums and same-origin download links', async () => {
    renderApp('/browse?repo=npm-internal&ns=@acme&name=ui-kit&tab=files');
    const table = await screen.findByRole('table', { name: 'Files' });
    const link = await within(table).findByRole('link', { name: /ui-kit-5\.3\.1\.tgz/ });
    expect(link).toHaveAttribute('href', '/repository/npm-internal/@acme/ui-kit/-/ui-kit-5.3.1.tgz');
    expect(within(table).getByRole('button', { name: /Copy SHA-256 of/ })).toBeInTheDocument();
  });

  it('lists the repositories that hold the version and the groups that serve it', async () => {
    renderApp('/browse?repo=npm-internal&ns=@acme&name=ui-kit&tab=repositories');
    const table = await screen.findByRole('table', { name: 'Repositories with this version' });
    expect(within(table).getByRole('link', { name: 'npm-internal' })).toHaveAttribute('aria-current', 'page');
    expect(screen.getByRole('link', { name: 'npm-all' })).toBeInTheDocument();
  });

  it('deletes a version after confirmation and moves to the newest remaining one', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/browse?repo=docker-prod&name=payments-api&version=2.14.0');
    await screen.findByRole('heading', { level: 1, name: 'payments-api' });
    await user.click(screen.getByRole('button', { name: 'Delete version' }));
    const dialog = screen.getByRole('alertdialog', { name: 'Delete payments-api 2.14.0?' });
    expect(within(dialog).getByRole('button', { name: 'Cancel' })).toHaveFocus();
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument();
    expect(catalogState.components.some((c) => c.id === 'c_8812')).toBe(true);

    await user.click(screen.getByRole('button', { name: 'Delete version' }));
    await user.click(within(screen.getByRole('alertdialog')).getByRole('button', { name: 'Delete version' }));
    await waitFor(() => expect(catalogState.components.some((c) => c.id === 'c_8812')).toBe(false));
    await waitFor(() => expect(search(history).get('version')).toBeNull());
    expect(await screen.findByRole('region', { name: 'payments-api 2.13.2' })).toBeInTheDocument();
  });

  it('disables deletion without delete rights on the repository', async () => {
    server.use(http.get('*/api/v1/auth/session', () => HttpResponse.json({ ...session, privileges: ['repo:*:read'] })));
    renderApp('/browse?repo=docker-prod&name=payments-api&tab=versions');
    await screen.findByRole('heading', { level: 1, name: 'payments-api' });
    expect(screen.getByRole('button', { name: 'Delete version' })).toBeDisabled();
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
    expect(screen.getByRole('region', { name: 'Version' })).toHaveTextContent('1 high');
    await user.click(screen.getByRole('tab', { name: 'Dependents' }));
    expect(within(screen.getByRole('table', { name: 'Dependents' })).getByRole('link', { name: '@acme/ui-kit 5.3.1' })).toBeInTheDocument();
  });

  it('says when the server does not read READMEs', async () => {
    server.use(http.get('*/api/v1/components/:id/info', () => HttpResponse.json({ code: 'not_implemented', message: 'no' }, { status: 501 })));
    renderApp('/browse?repo=npm-internal&ns=@acme&name=ui-kit');
    expect(await screen.findByText('This server does not read package READMEs.')).toBeInTheDocument();
    expect(screen.getByRole('region', { name: 'Version' })).toHaveTextContent('MIT');
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

describe('package browser: files', () => {
  it('shows a file and deletes it after confirmation', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/browse?repo=nuget-internal&path=acme.core/1.2.0/');
    await user.click(await item(/^acme\.core\.1\.2\.0\.nupkg/));
    await waitFor(() => expect(search(history).get('asset')).toBe('c_nuget_a0'));
    const head = await screen.findByRole('region', { name: 'File acme.core.1.2.0.nupkg' });
    expect(within(head).getByText('v3-flatcontainer/acme.core/1.2.0/acme.core.1.2.0.nupkg', { selector: '.pkg-head__meta' })).toBeInTheDocument();
    expect(within(head).getByRole('link', { name: 'acme.core 1.2.0' })).toBeInTheDocument();
    expect(within(head).getByRole('link', { name: /Download/ })).toHaveAttribute('href', '/repository/nuget-internal/v3-flatcontainer/acme.core/1.2.0/acme.core.1.2.0.nupkg');
    expect(within(head).getByRole('button', { name: 'Copy sha256' })).toBeInTheDocument();
    expect(within(head).getByRole('button', { name: 'Copy sha1' })).toBeInTheDocument();

    await user.click(within(head).getByRole('button', { name: 'Delete file' }));
    const dialog = screen.getByRole('alertdialog', { name: 'Delete acme.core.1.2.0.nupkg?' });
    await user.click(within(dialog).getByRole('button', { name: 'Delete file' }));
    await waitFor(() => expect(catalogState.deletedAssets.has('c_nuget_a0')).toBe(true));
    await waitFor(() => expect(search(history).get('asset')).toBeNull());
    expect(search(history).get('path')).toBe('acme.core/1.2.0/');
    await waitFor(() => expect(within(tree()).queryByRole('treeitem', { name: /^acme\.core\.1\.2\.0\.nupkg/ })).toBeNull());
  });

  it('marks a cached file of a proxy and disables deletion without rights', async () => {
    server.use(http.get('*/api/v1/auth/session', () => HttpResponse.json({ ...session, privileges: ['repo:*:read'] })));
    renderApp('/browse?repo=maven-central&path=org/apache/logging/log4j/log4j-core/2.24.1/log4j-core-2.24.1.jar&asset=c_log4j_a0');
    const head = await screen.findByRole('region', { name: 'File log4j-core-2.24.1.jar' });
    expect(within(head).getByText('Cached from upstream')).toBeInTheDocument();
    expect(within(head).getByText('https://repo1.maven.org/maven2/')).toBeInTheDocument();
    expect(within(head).getByRole('button', { name: 'Delete file' })).toBeDisabled();
  });

  it('says when a file is gone', async () => {
    renderApp('/browse?repo=npm-internal&path=x/y.tgz&asset=missing');
    expect(await screen.findByText('This file no longer exists, or you cannot see it.')).toBeInTheDocument();
  });
});
