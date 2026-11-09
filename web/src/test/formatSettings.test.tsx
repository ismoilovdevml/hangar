import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';
import type { RepositoryInput } from '../api/types';
import { session } from '../mocks/data';
import { mockState } from '../mocks/handlers';
import { server } from '../mocks/node';
import { signingState } from '../mocks/signing';
import { choose } from './choose';
import { renderApp } from './render';

function capture(method: string, path: RegExp) {
  const bodies: unknown[] = [];
  server.events.on('request:start', async ({ request }) => {
    if (request.method === method && path.test(new URL(request.url).pathname)) {
      const text = await request.clone().text();
      bodies.push(text ? JSON.parse(text) : undefined);
    }
  });
  return bodies;
}

afterEach(() => server.events.removeAllListeners());

const settingsStep = () => screen.getByRole('region', { name: /Settings/ });
const asRead = (privileges: string[]) => server.use(http.get('*/api/v1/auth/session', () => HttpResponse.json({ ...session, privileges })));

describe('format settings in the repository form', () => {
  it.each([
    ['apt', 'hosted', ['Architectures', 'Sign with']],
    ['apt', 'group', ['Architectures', 'Sign with']],
    ['apt', 'proxy', []],
    ['rpm', 'hosted', ['Repodata depth', 'Sign with']],
    ['rpm', 'proxy', []],
    ['go', 'proxy', ['Checksum databases', 'Checksum requests go']],
    ['go', 'hosted', []],
    ['cargo', 'proxy', ['Crate download URL']],
    ['cargo', 'hosted', []],
    ['helm', 'proxy', ['Chart hosts']],
    ['helm', 'hosted', []],
    ['npm', 'proxy', []],
  ])('%s %s shows %j', async (format, type, labels) => {
    renderApp(`/repositories/new?format=${format}&type=${type}`);
    const step = await screen.findByRole('region', { name: /Settings/ });
    const all = ['Architectures', 'Sign with', 'Repodata depth', 'Checksum databases', 'Checksum requests go', 'Crate download URL', 'Chart hosts'];
    for (const l of all) {
      const re = new RegExp(`^${l}`);
      if (labels.includes(l)) expect(within(step).getByLabelText(re)).toBeInTheDocument();
      else expect(within(step).queryByLabelText(re)).not.toBeInTheDocument();
    }
  });

  it('creates an APT repository with architectures and the shared key, refusing "all"', async () => {
    const posts = capture('POST', /^\/api\/v1\/repositories$/);
    const user = userEvent.setup();
    renderApp('/repositories/new?format=apt&type=hosted');
    const arch = await screen.findByLabelText(/^Architectures/);
    expect(screen.getByText(/Empty: amd64 and arm64/)).toBeInTheDocument();
    await user.type(arch, 'all{Enter}');
    expect(await screen.findByText(/Leave out "all"/)).toBeInTheDocument();
    expect(arch).toHaveAttribute('aria-invalid', 'true');
    await user.click(screen.getByRole('button', { name: 'Remove architecture all' }));
    await user.type(arch, 'amd64, i386{Enter}');
    expect(screen.queryByText(/Leave out "all"/)).not.toBeInTheDocument();
    await choose(user, screen.getByLabelText('Sign with'), 'shared');
    expect(screen.getByRole('button', { name: 'Copy repository configuration as YAML' }).parentElement).toHaveTextContent('signing_key: shared');
    await user.click(screen.getByRole('button', { name: 'Create repository' }));
    await waitFor(() => expect(posts).toHaveLength(1));
    expect((posts[0] as RepositoryInput).config.format).toEqual({ architectures: ['amd64', 'i386'], signing_key: 'shared' });
  });

  it('creates an RPM repository with a repodata depth from 0 to 4', async () => {
    const posts = capture('POST', /^\/api\/v1\/repositories$/);
    const user = userEvent.setup();
    renderApp('/repositories/new?format=rpm&type=hosted');
    const depth = await screen.findByLabelText('Repodata depth');
    expect(depth).toHaveValue(0);
    expect(within(settingsStep()).getByText('el9/x86_64/')).toBeInTheDocument();
    await user.clear(depth);
    await user.type(depth, '5');
    await user.tab();
    expect(screen.getByText('Enter a whole number from 0 to 4.')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Create repository' }));
    expect(posts).toHaveLength(0);
    expect(depth).toHaveFocus();
    await user.clear(depth);
    await user.type(depth, '2');
    await user.click(screen.getByRole('button', { name: 'Create repository' }));
    await waitFor(() => expect(posts).toHaveLength(1));
    expect((posts[0] as RepositoryInput).config.format).toEqual({ repodata_depth: 2 });
  });

  it('creates a Go proxy with the checksum database pass-through off, via the remote', async () => {
    const posts = capture('POST', /^\/api\/v1\/repositories$/);
    const user = userEvent.setup();
    renderApp('/repositories/new?format=go&type=proxy');
    await screen.findByLabelText('Checksum databases');
    expect(screen.getByText(/GOPRIVATE/)).toBeInTheDocument();
    expect(screen.getByRole('list', { name: 'checksum databases' })).toHaveTextContent('sum.golang.org');
    await user.type(screen.getByLabelText('Checksum databases'), 'Sum.Example{Enter}');
    expect(await screen.findByText(/lowercase, such as sum.golang.org/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Remove checksum database Sum.Example' }));
    await user.click(screen.getByRole('button', { name: 'Remove checksum database sum.golang.org' }));
    await choose(user, screen.getByLabelText('Checksum requests go'), 'remote');
    await user.click(screen.getByRole('button', { name: 'Create repository' }));
    await waitFor(() => expect(posts).toHaveLength(1));
    expect((posts[0] as RepositoryInput).config.format).toEqual({ sumdbs: [], sumdb_via: 'remote' });
  });

  it('leaves the Go defaults out of an untouched proxy', async () => {
    const posts = capture('POST', /^\/api\/v1\/repositories$/);
    const user = userEvent.setup();
    renderApp('/repositories/new?format=go&type=proxy');
    await screen.findByLabelText('Checksum databases');
    await user.click(screen.getByRole('button', { name: 'Create repository' }));
    await waitFor(() => expect(posts).toHaveLength(1));
    expect((posts[0] as RepositoryInput).config).not.toHaveProperty('format');
  });

  it('creates a Cargo proxy with a download template and a Helm proxy with chart hosts', async () => {
    const posts = capture('POST', /^\/api\/v1\/repositories$/);
    const user = userEvent.setup();
    const first = renderApp('/repositories/new?format=cargo&type=proxy');
    const dl = await screen.findByLabelText(/^Crate download URL/);
    await user.type(dl, 'ftp://dl.example/{{crate}');
    await user.tab();
    expect(screen.getByText('Use an absolute http:// or https:// address.')).toBeInTheDocument();
    await user.clear(dl);
    await user.type(dl, 'https://dl.example/{{crate}/{{version}');
    await user.click(screen.getByRole('button', { name: 'Create repository' }));
    await waitFor(() => expect(posts).toHaveLength(1));
    expect((posts[0] as RepositoryInput).config.format).toEqual({ download_url: 'https://dl.example/{crate}/{version}' });
    first.unmount();

    renderApp('/repositories/new?format=helm&type=proxy');
    await user.type(await screen.findByLabelText('Remote URL'), 'https://charts.example.com');
    const hosts = screen.getByLabelText(/^Chart hosts/);
    await user.type(hosts, 'https://cdn.example.com{Enter}');
    expect(await screen.findByText(/host:port or .domain/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Remove host https://cdn.example.com' }));
    await user.type(hosts, '.cdn.example.com{Enter}');
    await user.click(screen.getByRole('button', { name: 'Create repository' }));
    await waitFor(() => expect(posts).toHaveLength(2));
    const helm = posts[1] as RepositoryInput;
    expect(helm.config.proxy?.related_hosts).toEqual(['.cdn.example.com']);
    expect(helm.config).not.toHaveProperty('format');
  });

  it('pre-fills the settings page and saves a changed depth with the stored key choice', async () => {
    const puts = capture('PUT', /^\/api\/v1\/repositories\/internal-rpm$/);
    const user = userEvent.setup();
    renderApp('/repositories/internal-rpm/settings');
    const depth = await screen.findByLabelText('Repodata depth');
    expect(depth).toHaveValue(2);
    expect(screen.getByLabelText('Sign with')).toHaveTextContent('Shared server key');
    await user.clear(depth);
    await user.type(depth, '1');
    await user.click(screen.getByRole('button', { name: 'Save changes' }));
    expect(await screen.findByRole('button', { name: 'Saved' })).toBeDisabled();
    expect((puts[0] as RepositoryInput).config.format).toEqual({ repodata_depth: 1, signing_key: 'shared' });
    expect(mockState.repositories.find((r) => r.name === 'internal-rpm')!.config.format).toEqual({ repodata_depth: 1, signing_key: 'shared' });
  });
});

describe('signing key panel', () => {
  const panel = () => screen.findByRole('region', { name: 'Signing key' });

  it('shows the key and where clients get the public key', async () => {
    renderApp('/repositories/internal-apt/settings');
    const p = await panel();
    expect(await within(p).findByText('RSA 4096')).toBeInTheDocument();
    expect(within(p).getByText('Own key')).toBeInTheDocument();
    expect(within(p).getByText('1 Jun 2026')).toBeInTheDocument();
    expect(within(p).getByText(/^[0-9A-F]{4}( [0-9A-F]{4}){9}$/)).toBeInTheDocument();
    expect(within(p).getByRole('link', { name: 'public.gpg' })).toHaveAttribute('href', 'https://pkg.acme.io/repository/internal-apt/public.gpg');
    expect(within(p).getByRole('link', { name: 'public.key' })).toHaveAttribute('href', 'https://pkg.acme.io/repository/internal-apt/public.key');
    expect(within(p).getByRole('button', { name: 'Copy public.gpg URL' })).toBeInTheDocument();
  });

  it('is not shown for proxies or formats without signing', async () => {
    renderApp('/repositories/go-proxy/settings');
    await screen.findByRole('form', { name: 'Settings of go-proxy' });
    expect(screen.queryByRole('region', { name: 'Signing key' })).not.toBeInTheDocument();
  });

  it('rotates only after a confirmation that explains clients must re-fetch the key', async () => {
    const rotations = capture('POST', /signing-key:rotate$/);
    const user = userEvent.setup();
    renderApp('/repositories/internal-apt/settings');
    const p = await panel();
    const before = (await within(p).findByText(/^[0-9A-F]{4}( [0-9A-F]{4}){9}$/)).textContent;
    await user.click(within(p).getByRole('button', { name: 'Rotate…' }));
    let dialog = screen.getByRole('alertdialog', { name: 'Rotate the signing key of internal-apt?' });
    expect(dialog).toHaveTextContent('not kept for an overlap');
    expect(dialog).toHaveTextContent('NO_PUBKEY');
    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    expect(rotations).toHaveLength(0);

    await user.click(within(p).getByRole('button', { name: 'Rotate…' }));
    dialog = screen.getByRole('alertdialog');
    await user.click(within(dialog).getByRole('button', { name: 'Rotate key' }));
    await waitFor(() => expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument());
    expect(rotations).toHaveLength(1);
    await waitFor(() => expect(within(p).getByText(/^[0-9A-F]{4}( [0-9A-F]{4}){9}$/).textContent).not.toBe(before));
    expect(screen.getByText('Signing key rotated')).toBeInTheDocument();
  });

  it('imports a pasted key write-only: sent once, never shown again', async () => {
    const imports = capture('PUT', /signing-key$/);
    const user = userEvent.setup();
    renderApp('/repositories/internal-apt/settings');
    const p = await panel();
    await within(p).findByText('RSA 4096');
    await user.click(within(p).getByRole('button', { name: 'Import key…' }));
    let dialog = screen.getByRole('alertdialog', { name: 'Import a signing key for internal-apt' });
    await user.click(within(dialog).getByRole('button', { name: 'Import key' }));
    expect(within(dialog).getByRole('alert')).toHaveTextContent('Paste the private key.');
    expect(imports).toHaveLength(0);

    await user.type(within(dialog).getByLabelText('Private key'), 'not a key');
    await user.click(within(dialog).getByRole('button', { name: 'Import key' }));
    expect(await within(dialog).findByRole('alert')).toHaveTextContent('no armored data found');

    const armored = '-----BEGIN PGP PRIVATE KEY BLOCK-----\nlQdGBGSECRET\n-----END PGP PRIVATE KEY BLOCK-----';
    await user.clear(within(dialog).getByLabelText('Private key'));
    await user.click(within(dialog).getByLabelText('Private key'));
    await user.paste(armored);
    const pass = within(dialog).getByLabelText(/Passphrase/);
    expect(pass).toHaveAttribute('type', 'password');
    await user.type(pass, 's3cret');
    await user.click(within(dialog).getByRole('button', { name: 'Import key' }));
    await waitFor(() => expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument());
    expect(imports.at(-1)).toEqual({ private_key: armored, passphrase: 's3cret' });
    expect(await within(p).findByText('Ed25519')).toBeInTheDocument();
    expect(document.body).not.toHaveTextContent('lQdGBGSECRET');

    await user.click(within(p).getByRole('button', { name: 'Import key…' }));
    dialog = screen.getByRole('alertdialog');
    expect(within(dialog).getByLabelText('Private key')).toHaveValue('');
    expect(within(dialog).getByLabelText(/Passphrase/)).toHaveValue('');
  });

  it('explains that the server needs HANGAR_SECRET_KEY when it answers 501', async () => {
    signingState.secretKey = false;
    renderApp('/repositories/internal-apt/settings');
    const p = await panel();
    expect(await within(p).findByText(/HANGAR_SECRET_KEY/)).toBeInTheDocument();
    expect(within(p).queryByRole('button', { name: 'Rotate…' })).not.toBeInTheDocument();
    expect(within(p).queryByRole('button', { name: 'Import key…' })).not.toBeInTheDocument();
  });

  it('offers a retry when the key cannot be loaded', async () => {
    server.use(http.get('*/api/v1/repositories/:name/signing-key', () => HttpResponse.json({ code: 'internal', message: 'internal error' }, { status: 500 })));
    renderApp('/repositories/internal-apt/settings');
    const p = await panel();
    expect(await within(p).findByText('Could not load the signing key.')).toBeInTheDocument();
    expect(within(p).getByRole('button', { name: 'Retry' })).toBeInTheDocument();
  });

  it('needs system:repos:admin for the shared key and repository admin for its own', async () => {
    asRead(['repo:*:admin']);
    const first = renderApp('/repositories/internal-rpm/settings');
    let p = await panel();
    expect(await within(p).findByText('Shared key')).toBeInTheDocument();
    await within(p).findByText('RSA 4096');
    expect(within(p).getByRole('button', { name: 'Rotate…' })).toBeDisabled();
    expect(within(p).getByRole('button', { name: 'Import key…' })).toBeDisabled();
    expect(within(p).getByText('Changing the shared key needs system:repos:admin.')).toBeInTheDocument();
    first.unmount();

    renderApp('/repositories/internal-apt/settings');
    p = await panel();
    await within(p).findByText('RSA 4096');
    expect(within(p).getByRole('button', { name: 'Rotate…' })).toBeEnabled();
  });

  it('lets readers see the key but not change it', async () => {
    asRead(['repo:*:read']);
    renderApp('/repositories/internal-apt/settings');
    const p = await panel();
    await within(p).findByText('RSA 4096');
    expect(within(p).getByRole('button', { name: 'Rotate…' })).toBeDisabled();
    expect(within(p).getByText('Changing the key needs admin rights on this repository.')).toBeInTheDocument();
    expect(within(p).getByRole('link', { name: 'public.gpg' })).toBeInTheDocument();
  });
});
