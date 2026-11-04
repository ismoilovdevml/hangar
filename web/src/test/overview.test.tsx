import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { renderApp } from './render';

describe('overview', () => {
  it('renders KPI cards from the metrics API', async () => {
    renderApp('/');
    const kpis = await screen.findByRole('region', { name: 'Key metrics' });
    await within(kpis).findByText('4.82');
    const storage = within(kpis).getByRole('article', { name: 'Storage used' });
    expect(within(storage).getByText('TB of 12 TB')).toBeInTheDocument();
    expect(within(storage).getByText('40%')).toBeInTheDocument();
    expect(within(storage).getByText('38 GB added since yesterday')).toBeInTheDocument();
    expect(within(storage).getByRole('meter', { name: 'Storage used' })).toHaveAttribute('aria-valuenow', '40');

    const requests = within(kpis).getByRole('article', { name: 'Requests today' });
    expect(within(requests).getByText('18.4M')).toBeInTheDocument();
    expect(within(requests).getByText('+6%')).toBeInTheDocument();

    const cache = within(kpis).getByRole('article', { name: 'Served from cache' });
    expect(within(cache).getByText('94.2')).toBeInTheDocument();
    expect(within(cache).getByText('Healthy')).toBeInTheDocument();
    expect(within(cache).getByText(/^45\.4K of 48\.2K requests · \d+(\.\d)? GB saved$/)).toBeInTheDocument();

    const issues = within(kpis).getByRole('article', { name: 'Critical issues' });
    expect(within(issues).getByText('7')).toBeInTheDocument();
    expect(within(issues).getByText('in 3 packages')).toBeInTheDocument();
    expect(within(issues).getByText('2 already quarantined')).toBeInTheDocument();

    expect(screen.getByText('Everything is calm. 214 packages published since yesterday, no failed pushes.')).toBeInTheDocument();
  });

  it('renders recent packages, upstreams and client snippets', async () => {
    renderApp('/');
    const table = await screen.findByRole('table', { name: 'Recently published packages' });
    expect(await within(table).findByText('payments-api')).toBeInTheDocument();
    expect(within(table).getByText('1 critical')).toBeInTheDocument();
    expect(within(table).getByText('2 high')).toBeInTheDocument();
    expect(within(table).getByText('Just now')).toBeInTheDocument();

    const upstreams = screen.getByRole('region', { name: 'Upstream sources' });
    expect(await within(upstreams).findByText('npmjs-proxy')).toBeInTheDocument();
    expect(within(upstreams).getByText('Blocked')).toBeInTheDocument();
    expect(within(upstreams).getByText('Unknown')).toBeInTheDocument();
    expect(within(upstreams).getByText('Median latency · today')).toBeInTheDocument();
    expect(within(upstreams).getByText('412 ms')).toHaveAttribute('title', 'Median of 860 remote requests');
    // A remote without requests in the range: no latency, and why.
    const pypi = within(upstreams).getByText('pypi-proxy').closest('li')!;
    expect(within(pypi).getByTitle('No remote requests in this range')).toHaveTextContent('—');

    expect(await screen.findByText('docker login cr.acme.io')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Copy client setup commands' })).toBeInTheDocument();
  });

  it('keeps the selected range in the URL', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/');
    const group = await screen.findByRole('radiogroup', { name: 'Time range' });
    expect(within(group).getByRole('radio', { name: 'Today' })).toHaveAttribute('aria-checked', 'true');
    await user.click(within(group).getByRole('radio', { name: '7 days' }));
    await waitFor(() => expect(history.location.search).toContain('range=7d'));
    expect(await screen.findByRole('article', { name: 'Requests · 7 days' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Traffic · 7 days' })).toBeInTheDocument();
    // Upstream latency follows the range.
    const upstreams = screen.getByRole('region', { name: 'Upstream sources' });
    expect(await within(upstreams).findByText('Median latency · 7 days')).toBeInTheDocument();
    expect(await within(upstreams).findByTitle('Median of 5.6K remote requests')).toHaveTextContent('412 ms');
  });
});
