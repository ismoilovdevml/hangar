import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it, vi } from 'vitest';
import { overview, session } from '../mocks/data';
import { findingsState } from '../mocks/findings';
import { server } from '../mocks/node';
import { renderApp } from './render';

const search = (h: { location: { search: string } }) => new URLSearchParams(h.location.search);

describe('vulnerabilities', () => {
  const row = (name: string) => screen.getByText(name, { selector: '.vplan__name' }).closest('tr')!;
  const plan = () => screen.findByRole('table', { name: 'Fix plan by package' });

  it('shows the counts, the impact of upgrading and one row per package, used ones first', async () => {
    renderApp('/vulnerabilities');
    expect(await screen.findByRole('heading', { level: 1, name: 'Vulnerabilities' })).toBeInTheDocument();
    const pills = screen.getByRole('list', { name: 'Open findings by severity' });
    await waitFor(() => expect(within(pills).getByText('5 high')).toBeInTheDocument());
    expect(within(pills).getByText('1 critical')).toBeInTheDocument();
    expect(await screen.findByText(/9 scanned · OSV · checked 40 min ago/)).toBeInTheDocument();

    const impact = await screen.findByRole('region', { name: 'Impact' });
    await waitFor(() => expect(impact).toHaveTextContent('Upgrading 4 packages fixes 5 of 8 findings that builds actually use.'));
    expect(impact).toHaveTextContent('The other 3 are in packages nobody downloaded in 30 days.');
    expect(within(impact).getByRole('img', { name: '5 findings in used packages, 3 in unused packages' })).toBeInTheDocument();

    const table = await plan();
    const names = within(table).getAllByText(/./, { selector: '.vplan__name' }).map((n) => n.textContent);
    expect(names.slice(0, 4)).toEqual(['com.acme:ledger-core', 'System.Security.Cryptography.Xml', 'Newtonsoft.Json', '@acme/ui-kit']);
    const xml = row('System.Security.Cryptography.Xml');
    expect(within(xml).getByText('8.0.2, 10.0.8')).toBeInTheDocument();
    expect(within(xml).getByText('2 high · 8.1')).toBeInTheDocument();
    expect(within(xml).getByText('→ 8.0.4 / 10.0.10')).toBeInTheDocument();
    expect(within(xml).getByText('GHSA-g8r8-53c2-pm3f · CVE-2026-47304')).toBeInTheDocument();
    expect(xml).toHaveTextContent('14 by 2 clients');
    expect(within(row('Newtonsoft.Json')).getByRole('button', { name: /assigned to j\.karimov/ })).toBeInTheDocument();
    // Unused packages are dimmed and last.
    const unused = row('System.Net.Http');
    expect(unused).toHaveClass('is-unused');
    expect(unused).toHaveTextContent('0 not used');
    expect(within(row('org.apache.logging.log4j:log4j-core')).getByText('No fix yet')).toBeInTheDocument();
  });

  it('switches tabs and filters in the URL, with empty states', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/vulnerabilities');
    const table = await plan();
    await within(table).findByText('System.Net.Http');

    await user.click(screen.getByRole('tab', { name: 'All 7 packages' }));
    await waitFor(() => expect(search(history).get('tab')).toBe('all'));
    expect(screen.getByRole('tab', { name: 'All 7 packages' })).toHaveAttribute('aria-selected', 'true');
    // By severity alone: the unused high finding comes before the used medium one.
    await waitFor(() => {
      const names = within(table).getAllByText(/./, { selector: '.vplan__name' }).map((n) => n.textContent);
      expect(names.indexOf('System.Net.Http')).toBeLessThan(names.indexOf('org.apache.logging.log4j:log4j-core'));
      expect(names.indexOf('System.Net.Http')).toBeLessThan(names.indexOf('@acme/ui-kit'));
    });

    await user.click(screen.getByRole('tab', { name: 'Waived · 1' }));
    await waitFor(() => expect(search(history).get('tab')).toBe('waived'));
    await waitFor(() => expect(within(table).getAllByText(/./, { selector: '.vplan__name' }).map((n) => n.textContent)).toEqual(['@acme/ui-kit']));
    expect(within(table).queryByRole('button', { name: /^Waive / })).not.toBeInTheDocument();

    // Assigned to me: nothing yet.
    await user.click(screen.getByRole('tab', { name: 'In use first' }));
    await user.click(screen.getByRole('button', { name: 'Assigned to me' }));
    await waitFor(() => expect(search(history).get('mine')).toBe('true'));
    expect(await within(table).findByText('Nothing is assigned to you.')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Assigned to me' }));

    await user.type(screen.getByRole('searchbox', { name: 'Search packages and advisories' }), 'CVE-2024-21907');
    await waitFor(() => expect(search(history).get('q')).toBe('CVE-2024-21907'));
    await waitFor(() => expect(within(table).getAllByText(/./, { selector: '.vplan__name' }).map((n) => n.textContent)).toEqual(['Newtonsoft.Json']));
    await user.clear(screen.getByRole('searchbox', { name: 'Search packages and advisories' }));
    await user.type(screen.getByRole('searchbox', { name: 'Search packages and advisories' }), 'nothing-like-this');
    expect(await within(table).findByText(/No package matches these filters/)).toBeInTheDocument();
  });

  it('expands a package into its findings and opens one in the drawer', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/vulnerabilities');
    const table = await plan();
    const toggle = await within(table).findByRole('button', { name: 'Show the 2 findings of System.Security.Cryptography.Xml' });
    await user.click(toggle);
    expect(toggle).toHaveAttribute('aria-expanded', 'true');
    await waitFor(() => expect(search(history).get('open')).toContain('nuget:System.Security.Cryptography.Xml'));
    const list = within(table).getByRole('list', { name: 'Findings of System.Security.Cryptography.Xml' });
    expect(within(list).getAllByRole('button')).toHaveLength(2);
    expect(within(list).getByText('System.Security.Cryptography.Xml@10.0.8')).toBeInTheDocument();

    await user.click(within(list).getByText('System.Security.Cryptography.Xml@10.0.8').closest('button')!);
    const drawer = await screen.findByRole('dialog', { name: '.NET XML encryption security feature bypass' });
    expect(search(history).get('finding')).toBe('f_8');
    expect(within(drawer).getByText('GHSA-g8r8-53c2-pm3f · CVE-2026-47304')).toBeInTheDocument();
    const fix = within(drawer).getByRole('note', { name: 'How to fix' });
    expect(fix).toHaveTextContent('Upgrade System.Security.Cryptography.Xml to 10.0.10 or later.');
    expect(within(fix).getByText('dotnet add package System.Security.Cryptography.Xml --version 10.0.10')).toBeInTheDocument();
    await user.click(within(fix).getByRole('button', { name: 'Copy the upgrade command' }));
    expect(await navigator.clipboard.readText()).toBe('dotnet add package System.Security.Cryptography.Xml --version 10.0.10');
    expect(within(fix).getByRole('button', { name: 'Copy the upgrade command' })).toHaveTextContent('Copied');

    const who = within(drawer).getByRole('region', { name: 'Who pulls it, last 7 days' });
    expect(await within(who).findByText('ci-publisher')).toBeInTheDocument();
    expect(within(who).getByText('2 pulls')).toBeInTheDocument();
    expect(within(drawer).getByText('>=8.0.0, <8.0.4; >=10.0.0, <10.0.10')).toBeInTheDocument();
    expect(within(drawer).getByRole('link', { name: /Full advisory on osv\.dev/ })).toHaveAttribute('href', 'https://osv.dev/vulnerability/GHSA-g8r8-53c2-pm3f');

    await user.keyboard('{Escape}');
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(search(history).get('finding')).toBeNull();
  });

  it('copies the upgrade commands of a package', async () => {
    const user = userEvent.setup();
    renderApp('/vulnerabilities');
    await plan();
    const copy = await screen.findByRole('button', { name: 'Copy the upgrade command for System.Security.Cryptography.Xml' });
    await user.click(copy);
    expect(await navigator.clipboard.readText()).toBe(
      'dotnet add package System.Security.Cryptography.Xml --version 8.0.4\ndotnet add package System.Security.Cryptography.Xml --version 10.0.10',
    );
    expect(copy).toHaveTextContent('Copied');
  });

  it('waives one finding of a package with a reason and an end date', async () => {
    const user = userEvent.setup();
    renderApp('/vulnerabilities');
    await plan();
    await user.click(await screen.findByRole('button', { name: 'Waive System.Security.Cryptography.Xml' }));
    const dialog = screen.getByRole('dialog', { name: 'Waive System.Security.Cryptography.Xml' });
    expect(within(dialog).getByRole('radio', { name: 'All 2 findings of this package' })).toBeChecked();
    await user.click(within(dialog).getByRole('radio', { name: 'GHSA-g8r8-53c2-pm3f on 10.0.8' }));
    await user.click(within(dialog).getByRole('button', { name: 'Waive' }));
    expect(within(dialog).getByText(/Say why this risk is accepted/)).toBeInTheDocument();
    await user.type(within(dialog).getByLabelText('Reason'), 'Only the 8.x line ships; 10.x is a test build');
    await user.click(within(dialog).getByRole('button', { name: 'Waive' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    const waived = findingsState.findings.filter((f) => f.status === 'waived').map((f) => f.id);
    expect(waived.sort()).toEqual(['f_3', 'f_8']);
    expect(Date.parse(findingsState.findings.find((f) => f.id === 'f_8')!.waiver_until!) - Date.now()).toBeGreaterThan(29 * 86_400_000);
    await waitFor(() => expect(within(row('System.Security.Cryptography.Xml')).getByText('1 high · 8.1')).toBeInTheDocument());
    expect(screen.getByRole('tab', { name: 'Waived · 2' })).toBeInTheDocument();
  });

  it('assigns a package and a finding, and filters to my assignments', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/vulnerabilities');
    await plan();
    await user.click(await screen.findByRole('button', { name: 'Assign System.Security.Cryptography.Xml' }));
    let dialog = screen.getByRole('dialog', { name: 'Assign System.Security.Cryptography.Xml' });
    await user.click(within(dialog).getByRole('button', { name: 'Assign' }));
    expect(within(dialog).getByText('Pick a user.')).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Assign to me (d.ismoilov)' }));
    await user.click(within(dialog).getByRole('button', { name: 'Assign' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(findingsState.findings.filter((f) => f.assignee === 'd.ismoilov').map((f) => f.id)).toEqual(['f_7', 'f_8']);
    expect(await screen.findByRole('button', { name: /Reassign System\.Security\.Cryptography\.Xml/ })).toHaveTextContent('d.ismoilov');

    await user.click(screen.getByRole('button', { name: 'Assigned to me' }));
    await waitFor(() => expect(search(history).get('mine')).toBe('true'));
    const table = await plan();
    await waitFor(() => expect(within(table).getAllByText(/./, { selector: '.vplan__name' }).map((n) => n.textContent)).toEqual(['System.Security.Cryptography.Xml']));

    // One finding from the drawer: pick another user, then remove them.
    await user.click(screen.getByRole('button', { name: 'Assigned to me' }));
    await user.click(await within(table).findByRole('button', { name: 'Show the 1 finding of Newtonsoft.Json' }));
    await user.click(within(table).getByText('Newtonsoft.Json@11.0.1').closest('button')!);
    const drawer = await screen.findByRole('dialog', { name: 'Improper handling of exceptional conditions in Newtonsoft.Json' });
    expect(within(drawer).getByText('Assigned to j.karimov')).toBeInTheDocument();
    await user.click(within(drawer).getByRole('button', { name: 'Reassign' }));
    dialog = screen.getByRole('dialog', { name: 'Assign GHSA-5crp-9r3c-p9vr' });
    await user.type(within(dialog).getByLabelText('Find a user'), 'malika');
    await user.click(await within(dialog).findByRole('radio', { name: /m\.nazarova/ }));
    await user.click(within(dialog).getByRole('button', { name: 'Assign' }));
    await waitFor(() => expect(findingsState.findings.find((f) => f.id === 'f_9')!.assignee).toBe('m.nazarova'));
    expect(await within(drawer).findByText('Assigned to m.nazarova')).toBeInTheDocument();
    await user.click(within(drawer).getByRole('button', { name: 'Reassign' }));
    await user.click(within(screen.getByRole('dialog', { name: 'Assign GHSA-5crp-9r3c-p9vr' })).getByRole('button', { name: 'Remove m.nazarova' }));
    await waitFor(() => expect(findingsState.findings.find((f) => f.id === 'f_9')!.assignee).toBeNull());
  });

  it('rescans every repository and shows the progress', async () => {
    const user = userEvent.setup();
    renderApp('/vulnerabilities');
    const rescan = await screen.findByRole('button', { name: 'Rescan' });
    await user.click(rescan);
    expect(await screen.findByRole('button', { name: 'Rescanning 0%' })).toBeDisabled();
    expect(screen.getByText('Rescanning · 0 of 12 packages')).toBeInTheDocument();
    expect(screen.getByRole('progressbar', { name: 'Rescan progress' })).toHaveAttribute('aria-valuenow', '0');
    expect(findingsState.rescan?.state).toBe('running');
  });

  it('exports the fix plan as CSV', async () => {
    const user = userEvent.setup();
    const blobs: Blob[] = [];
    const create = vi.fn((b: Blob) => {
      blobs.push(b);
      return 'blob:fix-plan';
    });
    Object.assign(URL, { createObjectURL: create, revokeObjectURL: vi.fn() });
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {});
    renderApp('/vulnerabilities?tab=all');
    await plan();
    await user.click(await screen.findByRole('button', { name: 'Export CSV' }));
    await waitFor(() => expect(click).toHaveBeenCalled());
    const text = await blobs[0]!.text();
    expect(text.split('\n')[0]).toMatch(/^package,format,repositories,advisories/);
    expect(text).toContain('Newtonsoft.Json,nuget,nuget-proxy');
    click.mockRestore();
  });

  it('hides the write actions and who pulls without the security privileges', async () => {
    server.use(http.get('*/api/v1/auth/session', () => HttpResponse.json({ ...session, privileges: ['repo:*:read'] })));
    const user = userEvent.setup();
    renderApp('/vulnerabilities?finding=f_7');
    const drawer = await screen.findByRole('dialog', { name: '.NET XML encryption security feature bypass' });
    expect(await within(drawer).findByText('Seeing who pulls a package needs the security read privilege.')).toBeInTheDocument();
    expect(within(drawer).getByText('Assigning and waiving need the security write privilege.')).toBeInTheDocument();
    expect(within(drawer).queryByRole('button', { name: 'Assign' })).not.toBeInTheDocument();
    await user.click(within(drawer).getByRole('button', { name: 'Close' }));
    const table = await plan();
    await within(table).findByText('Newtonsoft.Json');
    expect(within(table).queryByRole('button', { name: /^Waive / })).not.toBeInTheDocument();
    expect(within(table).queryByRole('button', { name: /^Assign / })).not.toBeInTheDocument();
    expect(within(row('Newtonsoft.Json')).getByText('j.karimov')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Rescan' })).not.toBeInTheDocument();
  });

  it('says when scanning is off and when the plan cannot be loaded', async () => {
    findingsState.enabled = false;
    findingsState.planFails = true;
    const user = userEvent.setup();
    renderApp('/vulnerabilities');
    expect(await screen.findByText('Scanning is turned off on this server')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Rescan' })).not.toBeInTheDocument();
    expect(await screen.findByText('Could not load the fix plan.')).toBeInTheDocument();
    findingsState.planFails = false;
    await user.click(screen.getByRole('button', { name: 'Retry' }));
    expect(await plan()).toBeInTheDocument();
  });

  it('is honest on the overview card before any scan', async () => {
    server.use(
      http.get('*/api/v1/metrics/overview', () => {
        const m = overview('today');
        return HttpResponse.json({ ...m, issues: { critical: 0, packages_affected: 0, quarantined: 0, open_findings: 0, scanning: { enabled: true, scanned_components: 0, last_scan_at: null } } });
      }),
    );
    renderApp('/');
    const card = () => screen.getByRole('link', { name: /^Security/ });
    await waitFor(() => expect(within(card()).getByText('Not scanned yet')).toBeInTheDocument());
    expect(within(card()).queryByText('Clean')).not.toBeInTheDocument();
    expect(within(card()).getByText('No package has been scanned yet')).toBeInTheDocument();
  });

  it('labels Docker images as not scanned on the package page', async () => {
    renderApp('/browse?repo=docker-prod&name=payments-api&version=2.14.0');
    await screen.findByRole('heading', { level: 1, name: 'payments-api' });
    const head = screen.getByRole('region', { name: 'payments-api 2.14.0' });
    const tabs = within(head).getByRole('tablist', { name: 'Package details' });
    // No empty "Vulnerabilities" tab for an image nobody scanned.
    await waitFor(() => expect(within(tabs).getAllByRole('tab').map((t) => t.textContent)).toContain('Versions 3'));
    await new Promise((r) => setTimeout(r, 50));
    expect(within(tabs).queryByRole('tab', { name: 'Vulnerabilities' })).not.toBeInTheDocument();
    expect(within(screen.getByRole('region', { name: 'Version' })).getByText('Image scanning not configured')).toBeInTheDocument();
  });

  it('shows image findings from Trivy with rebuild advice', async () => {
    findingsState.images = true;
    const user = userEvent.setup();
    renderApp('/vulnerabilities');
    expect(await screen.findByText(/9 scanned · OSV \+ Trivy · checked 40 min ago/)).toBeInTheDocument();
    const table = await plan();
    const image = await waitFor(() => row('auth-gateway'));
    expect(within(image).getByText('3.2.0')).toBeInTheDocument();
    expect(within(image).getByText('Rebuild · 3 packages')).toBeInTheDocument();
    expect(within(image).getByText('1 without a fix')).toBeInTheDocument();
    await user.click(within(image).getByRole('button', { name: 'Copy the Dockerfile line for auth-gateway' }));
    expect(await navigator.clipboard.readText()).toBe('RUN apk upgrade --no-cache libcrypto3 libssl3');

    await user.click(within(table).getByRole('button', { name: 'Show the 4 findings of auth-gateway' }));
    const list = within(table).getByRole('list', { name: 'Findings of auth-gateway' });
    expect(within(list).getByText('stdlib@1.22.3')).toBeInTheDocument();
    expect(within(list).getByText('usr/local/bin/auth-gateway · layer 7e1f2c3d4b5a · linux/amd64, linux/arm64')).toBeInTheDocument();
    expect(within(list).getAllByText('docker-prod · 3.2.0')).toHaveLength(4);
    expect(
      within(table).getByText(
        'Rebuild on an updated alpine 3.20.2 image, or upgrade in the Dockerfile: libcrypto3 3.3.2-r0 · libssl3 3.3.2-r0. Upgrade in the application, then rebuild: stdlib 1.22.4.',
      ),
    ).toBeInTheDocument();

    await user.click(within(list).getByText('libssl3@3.3.1-r0').closest('button')!);
    const drawer = await screen.findByRole('dialog', { name: 'openssl: Possible denial of service in X.509 name checks' });
    const fix = within(drawer).getByRole('note', { name: 'How to fix' });
    expect(fix).toHaveTextContent('Rebuild auth-gateway:3.2.0 on a base image that ships libssl3 3.3.2-r0 or later, or upgrade it in the Dockerfile.');
    expect(within(fix).getByText('RUN apk upgrade --no-cache libssl3')).toBeInTheDocument();
    expect(within(drawer).getByText('alpine 3.20.2 · layer 396c31837116 · linux/amd64, linux/arm64')).toBeInTheDocument();
    expect(within(drawer).getByRole('link', { name: 'auth-gateway:3.2.0' })).toBeInTheDocument();
    expect(within(drawer).getByRole('link', { name: /Full advisory/ })).toHaveAttribute('href', 'https://avd.aquasec.com/nvd/cve-2024-6119');
  });

  it('explains image findings without a fix and lists them on the image page', async () => {
    findingsState.images = true;
    const user = userEvent.setup();
    renderApp('/vulnerabilities?finding=f_i3');
    const drawer = await screen.findByRole('dialog', { name: 'busybox: use-after-free in awk' });
    expect(within(drawer).getByRole('note', { name: 'How to fix' })).toHaveTextContent('alpine 3.20.2 has no fixed busybox yet.');
    await user.keyboard('{Escape}');

    renderApp('/browse?repo=docker-prod&name=auth-gateway&version=3.2.0');
    await user.click(await screen.findByRole('tab', { name: 'Vulnerabilities' }));
    const vulns = screen.getByRole('table', { name: 'Vulnerabilities' });
    expect(within(vulns).getByText('stdlib@1.22.3')).toBeInTheDocument();
    expect(within(vulns).getByText('usr/local/bin/auth-gateway · layer 7e1f2c3d4b5a · linux/amd64, linux/arm64')).toBeInTheDocument();
  });

  it('shows the findings of a scanned package', async () => {
    const user = userEvent.setup();
    renderApp('/browse?repo=npm-internal&ns=%40acme&name=ui-kit&version=5.3.0');
    await user.click(await screen.findByRole('tab', { name: 'Vulnerabilities' }));
    expect(within(screen.getByRole('table', { name: 'Vulnerabilities' })).getByText('GHSA-p6mc-m468-83gw')).toBeInTheDocument();
  });
});
