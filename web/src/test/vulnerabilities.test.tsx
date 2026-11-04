import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { overview, session } from '../mocks/data';
import { findingsState } from '../mocks/findings';
import { server } from '../mocks/node';
import { renderApp } from './render';

const search = (h: { location: { search: string } }) => new URLSearchParams(h.location.search);

describe('vulnerabilities', () => {
  it('shows severity counts, the trend, the scanner and the findings', async () => {
    renderApp('/vulnerabilities');
    expect(await screen.findByRole('heading', { level: 1, name: 'Vulnerabilities' })).toBeInTheDocument();
    await waitFor(() => expect(within(screen.getByRole('article', { name: 'Critical' })).getByText('1')).toBeInTheDocument());
    expect(within(screen.getByRole('article', { name: 'Critical' })).getByText(/All have a fix/)).toBeInTheDocument();
    const medium = screen.getByRole('article', { name: 'Medium' });
    expect(within(medium).getByText(/No fix released yet/)).toBeInTheDocument();

    const table = await screen.findByRole('table', { name: 'Findings' });
    await within(table).findByRole('button', { name: 'GHSA-ghhp-997w-qr28' });
    const rows = within(table).getAllByRole('row').slice(1);
    // Most severe first; fixed findings are hidden by default.
    expect(within(rows[0]!).getByText('CVE-2021-26701')).toBeInTheDocument();
    expect(within(rows[0]!).getByText('com.acme:ledger-core@7.0.0')).toBeInTheDocument();
    expect(within(rows[0]!).getByText('Open · quarantined')).toBeInTheDocument();
    expect(within(table).queryByText('GHSA-29mw-wpgm-hmr9')).not.toBeInTheDocument();
    const waived = within(table).getByRole('button', { name: 'GHSA-35jh-r3h4-6jhm' }).closest('tr')!;
    expect(within(waived).getByText(/^Waived until /)).toBeInTheDocument();
    const noFix = within(table).getByRole('button', { name: 'GHSA-xxjr-mmjv-4gpg' }).closest('tr')!;
    expect(within(noFix).getByText('No fix yet')).toBeInTheDocument();
    expect(screen.getByText('5 findings · 1 waived')).toBeInTheDocument();

    expect(screen.getByRole('img', { name: /unresolved findings today \(1 critical\)/ })).toBeInTheDocument();
    const scanner = screen.getByRole('region', { name: 'Scanner' });
    expect(within(scanner).getByText(/Advisories from OSV \(api\.osv\.dev\)/)).toBeInTheDocument();
    expect(within(scanner).getByText(/image scanning is not configured/)).toBeInTheDocument();
  });

  it('filters with keyboard-operable pickers and keeps filters in the URL', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/vulnerabilities');
    const table = await screen.findByRole('table', { name: 'Findings' });
    await within(table).findByRole('button', { name: 'GHSA-ghhp-997w-qr28' });

    const severity = screen.getByRole('combobox', { name: 'Severity' });
    expect(severity).toHaveTextContent('All');
    severity.focus();
    await user.keyboard('{ArrowDown}');
    expect(screen.getByRole('listbox')).toBeInTheDocument();
    await user.keyboard('{ArrowDown}{ArrowDown}{Enter}');
    await waitFor(() => expect(search(history).get('severity')).toBe('high'));
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument();
    expect(screen.getByRole('combobox', { name: 'Severity' })).toHaveTextContent('High');
    await waitFor(() => expect(within(table).queryByRole('button', { name: 'GHSA-ghhp-997w-qr28' })).not.toBeInTheDocument());
    expect(within(table).getByRole('button', { name: 'GHSA-p6mc-m468-83gw' })).toBeInTheDocument();

    // Status: waived only.
    await user.click(screen.getByRole('combobox', { name: 'Status' }));
    await user.click(screen.getByRole('option', { name: 'Waived' }));
    await waitFor(() => expect(search(history).get('status')).toBe('waived'));
    await within(table).findByRole('button', { name: 'GHSA-35jh-r3h4-6jhm' });
    expect(within(table).getAllByRole('row')).toHaveLength(2);

    // Escape closes without choosing.
    await user.click(screen.getByRole('combobox', { name: 'Fix' }));
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument();
    expect(search(history).get('fix')).toBeNull();

    // Nothing matches: a way back.
    await user.click(screen.getByRole('combobox', { name: 'Fix' }));
    await user.click(screen.getByRole('option', { name: 'No fix yet' }));
    expect(await within(table).findByText(/No findings match these filters/)).toBeInTheDocument();
    await user.click(within(table).getByRole('link', { name: 'Clear filters' }));
    await within(table).findByRole('button', { name: 'GHSA-ghhp-997w-qr28' });
  });

  it('opens a finding with its references and waives it with a reason and an end date', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/vulnerabilities');
    const table = await screen.findByRole('table', { name: 'Findings' });
    await user.click(await within(table).findByRole('button', { name: 'GHSA-p6mc-m468-83gw' }));
    const drawer = await screen.findByRole('dialog', { name: 'GHSA-p6mc-m468-83gw' });
    expect(search(history).get('finding')).toBe('f_2');
    expect(within(drawer).getByRole('link', { name: '@acme/ui-kit@5.3.0' })).toHaveAttribute('href', expect.stringContaining('/browse?'));
    expect(await within(drawer).findByText('CVSS:3.1/AV:N/AC:H/PR:N/UI:N/S:U/C:N/I:H/A:H')).toBeInTheDocument();
    expect(within(drawer).getByRole('link', { name: 'CVE-2020-8203' })).toHaveAttribute('href', 'https://osv.dev/vulnerability/CVE-2020-8203');
    const refs = within(drawer).getByRole('region', { name: 'References' });
    expect(within(refs).getByRole('link', { name: /github\.com/ })).toHaveAttribute('rel', 'noreferrer noopener');

    // The reason is required.
    await user.click(within(drawer).getByRole('button', { name: 'Waive finding' }));
    expect(within(drawer).getByText(/Say why this risk is accepted/)).toBeInTheDocument();
    expect(within(drawer).getByLabelText('Reason')).toHaveAttribute('aria-invalid', 'true');

    await user.type(within(drawer).getByLabelText('Reason'), 'Only the server build uses this helper');
    await user.click(within(drawer).getByRole('button', { name: 'Waive finding' }));
    expect(await within(drawer).findByRole('button', { name: 'Revoke waiver' })).toBeInTheDocument();
    expect(within(drawer).getByText('Only the server build uses this helper')).toBeInTheDocument();
    const f = findingsState.findings.find((x) => x.id === 'f_2')!;
    expect(f.status).toBe('waived');
    expect(Date.parse(f.waiver_until!) - Date.now()).toBeGreaterThan(29 * 86_400_000);

    await user.click(within(drawer).getByRole('button', { name: 'Revoke waiver' }));
    expect(await within(drawer).findByRole('button', { name: 'Waive finding' })).toBeInTheDocument();
    expect(findingsState.findings.find((x) => x.id === 'f_2')!.status).toBe('open');

    // Rescan the package.
    await user.click(within(drawer).getByRole('button', { name: 'Scan now' }));
    expect(await within(drawer).findByText(/Scanned: 2 known vulnerabilities, 0 new/)).toBeInTheDocument();

    await user.keyboard('{Escape}');
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(search(history).get('finding')).toBeNull();
  });

  it('hides waiver actions without the security privilege and scans a repository on demand', async () => {
    server.use(http.get('*/api/v1/auth/session', () => HttpResponse.json({ ...session, privileges: ['repo:npm-*:write', 'repo:*:read'] })));
    const user = userEvent.setup();
    renderApp('/vulnerabilities?repo=npm-internal&finding=f_2');
    const drawer = await screen.findByRole('dialog', { name: 'GHSA-p6mc-m468-83gw' });
    expect(await within(drawer).findByText(/Waivers need the security write privilege/)).toBeInTheDocument();
    expect(within(drawer).queryByRole('button', { name: 'Waive finding' })).not.toBeInTheDocument();
    await user.click(within(drawer).getByRole('button', { name: 'Close' }));

    const scanner = screen.getByRole('region', { name: 'Scanner' });
    await user.click(await within(scanner).findByRole('button', { name: 'Scan npm-internal now' }));
    expect(await within(scanner).findByText(/Queued a scan of 12 packages in npm-internal/)).toBeInTheDocument();
  });

  it('says when scanning is turned off', async () => {
    findingsState.enabled = false;
    renderApp('/vulnerabilities');
    const scanner = await screen.findByRole('region', { name: 'Scanner' });
    expect(await within(scanner).findByText(/Scanning is turned off on this server/)).toBeInTheDocument();
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

  it('shows the findings of a scanned package', async () => {
    const user = userEvent.setup();
    renderApp('/browse?repo=npm-internal&ns=%40acme&name=ui-kit&version=5.3.0');
    await user.click(await screen.findByRole('tab', { name: 'Vulnerabilities' }));
    expect(within(screen.getByRole('table', { name: 'Vulnerabilities' })).getByText('GHSA-p6mc-m468-83gw')).toBeInTheDocument();
  });
});
