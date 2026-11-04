import { choose } from './choose';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { session } from '../mocks/data';
import { mockState } from '../mocks/handlers';
import { server } from '../mocks/node';
import { opsMockState } from '../mocks/ops';
import { renderApp } from './render';

const search = (h: { location: { search: string } }) => new URLSearchParams(h.location.search);

describe('cleanup policies', () => {
  it('lists policies and opens the first one as readable sentences', async () => {
    renderApp('/cleanup');
    const list = await screen.findByRole('navigation', { name: 'Cleanup policies' });
    expect(await within(list).findByRole('link', { name: /keep-last-20/ })).toHaveAttribute('aria-current', 'page');
    expect(within(list).getByRole('link', { name: /docker-lean/ })).toHaveTextContent('3 repositories · Every day at 02:00 UTC');
    expect(within(list).getByRole('link', { name: /raw-build-outputs/ })).toHaveTextContent('Paused');

    expect(screen.getByRole('heading', { level: 2, name: 'keep-last-20' })).toBeInTheDocument();
    expect(screen.getByLabelText('Newest versions to keep')).toHaveValue(20);
    expect(screen.getByLabelText('Days without a download')).toHaveValue(90);
    expect(screen.getByLabelText('Days since published')).toBeDisabled();
    expect(screen.getByLabelText('Name')).toHaveAttribute('readonly');
    // Saving needs a fresh preview.
    expect(screen.getByRole('button', { name: 'Save policy' })).toBeDisabled();
  });

  it('previews, then creates a policy and shows it in the URL', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/cleanup?create=true');
    await screen.findByRole('heading', { level: 2, name: 'New policy' });

    await user.type(screen.getByLabelText('Name'), 'docker-dev-prune');
    await choose(user, screen.getByLabelText('Format'), 'docker');
    const days = screen.getByLabelText('Days since published');
    await user.clear(days);
    await user.type(days, '30');
    await user.click(screen.getByLabelText(/Use condition: and not downloaded/));
    await user.click(screen.getByLabelText(/Use condition: keep the newest/));
    await user.type(screen.getByLabelText('tags matching'), '3.*');
    await waitFor(() => expect(screen.getByLabelText('Add repository')).toBeEnabled());
    await choose(user, screen.getByLabelText('Add repository'), 'docker-dev');
    expect(screen.getByRole('button', { name: 'Remove docker-dev' })).toBeInTheDocument();
    expect(screen.getByText(/next 5 Nov 2026|next \d+ \w+ \d{4}/).closest('p')).toHaveTextContent('Every day at 03:00 UTC · 0 3 * * *');

    const create = screen.getByRole('button', { name: 'Create policy' });
    expect(create).toBeDisabled();
    await user.click(screen.getByRole('button', { name: 'Preview' }));
    // feat-sso-2 (110 days) and feat-sso-3 (95 days) are older than 30 days; 3.2.0 is kept by the glob.
    expect(await screen.findByRole('heading', { name: '2 versions · 418 MB would be freed' })).toBeInTheDocument();
    const table = screen.getByRole('table', { name: 'Versions this policy would delete' });
    expect(within(table).getByText('auth-gateway:feat-sso-3')).toBeInTheDocument();
    expect(within(table).queryByText('auth-gateway:3.2.0')).not.toBeInTheDocument();

    await user.click(create);
    await waitFor(() => expect(search(history).get('policy')).toBe('docker-dev-prune'));
    expect(await screen.findByText('Policy docker-dev-prune created.', { selector: '.notice' })).toBeInTheDocument();
    const saved = opsMockState.policies.find((p) => p.name === 'docker-dev-prune')!;
    expect(saved).toMatchObject({
      format: 'docker',
      criteria: { older_than_days: 30, keep_matching: ['3.*'] },
      repositories: ['docker-dev'],
      schedule: '0 3 * * *',
    });
  });

  it('marks the preview stale when conditions change and blocks saving', async () => {
    const user = userEvent.setup();
    renderApp('/cleanup?policy=docker-lean');
    await screen.findByRole('heading', { level: 2, name: 'docker-lean' });
    await user.click(screen.getByRole('button', { name: 'Preview' }));
    await screen.findByRole('heading', { name: /would be freed/ });
    // Nothing changed yet: saving is pointless.
    expect(screen.getByRole('button', { name: 'Save policy' })).toBeDisabled();

    const keep = screen.getByLabelText('Newest versions to keep');
    await user.clear(keep);
    await user.type(keep, '1');
    expect(screen.getByText('Preview · out of date, conditions changed')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Save policy' })).toBeDisabled();
    expect(screen.getByRole('button', { name: /Run now/ })).toBeDisabled();

    await user.click(screen.getByRole('button', { name: 'Preview' }));
    await screen.findByText('Preview · nothing is deleted yet');
    await waitFor(() => expect(screen.getByRole('button', { name: 'Save policy' })).toBeEnabled());
    await user.click(screen.getByRole('button', { name: 'Save policy' }));
    expect(await screen.findByText('Policy docker-lean saved.', { selector: '.notice' })).toBeInTheDocument();
    expect(opsMockState.policies.find((p) => p.name === 'docker-lean')!.criteria.keep_newest).toBe(1);
  });

  it('validates before previewing', async () => {
    const user = userEvent.setup();
    renderApp('/cleanup?create=true');
    await screen.findByRole('heading', { level: 2, name: 'New policy' });
    await user.click(screen.getByRole('button', { name: 'Preview' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Attach at least one repository');
  });

  it('describes a custom cron schedule in plain words and flags mistakes', async () => {
    const user = userEvent.setup();
    renderApp('/cleanup?create=true');
    await screen.findByRole('heading', { level: 2, name: 'New policy' });
    await choose(user, screen.getByLabelText('How often'), 'custom');
    const cron = screen.getByLabelText('Cron expression');
    expect(cron).toHaveValue('0 3 * * *');
    await user.clear(cron);
    await user.type(cron, '0 */6 * *');
    expect(screen.getByText(/Expected 5 fields/)).toBeInTheDocument();
    await user.type(cron, ' *');
    expect(screen.getByText(/Every 6 hours/)).toBeInTheDocument();
  });

  it('runs a policy after confirmation and lists the run', async () => {
    const user = userEvent.setup();
    renderApp('/cleanup?policy=docker-lean');
    const history = await screen.findByRole('table', { name: 'Latest runs of docker-lean' });
    expect(await within(history).findByText('Blob store s3-eu-primary unavailable')).toBeInTheDocument();
    expect(within(history).getByText('31 GB')).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: /Run now/ }));
    const dialog = screen.getByRole('alertdialog', { name: 'Run docker-lean now?' });
    expect(within(dialog).getByRole('button', { name: 'Cancel' })).toHaveFocus();
    await user.click(within(dialog).getByRole('button', { name: 'Run cleanup now' }));
    await waitFor(() => expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument());
    expect((await within(history).findAllByText('Queued')).length).toBeGreaterThan(0);
  });

  it('deletes a policy after confirmation', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/cleanup?policy=npm-proxy-cache');
    await screen.findByRole('heading', { level: 2, name: 'npm-proxy-cache' });
    await user.click(screen.getByRole('button', { name: 'Delete policy' }));
    await user.click(within(screen.getByRole('alertdialog')).getByRole('button', { name: 'Delete policy' }));
    await waitFor(() => expect(search(history).get('policy')).toBeNull());
    expect(opsMockState.policies.some((p) => p.name === 'npm-proxy-cache')).toBe(false);
    await waitFor(() =>
      expect(within(screen.getByRole('navigation', { name: 'Cleanup policies' })).queryByText('npm-proxy-cache')).not.toBeInTheDocument(),
    );
  });

  it('is read-only without the cleanup write privilege', async () => {
    server.use(
      http.get('*/api/v1/auth/session', () =>
        mockState.signedIn ? HttpResponse.json({ ...session, privileges: ['system:cleanup:read'] }) : HttpResponse.json({}, { status: 401 }),
      ),
    );
    renderApp('/cleanup?policy=docker-lean');
    await screen.findByRole('heading', { level: 2, name: 'docker-lean' });
    expect(screen.queryByRole('button', { name: 'Preview' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Save policy' })).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: /New policy/ })).not.toBeInTheDocument();
    expect(screen.getByLabelText('Newest versions to keep')).toBeDisabled();
    expect(screen.getByText(/needs the cleanup write privilege/)).toBeInTheDocument();
  });

  it('says when a linked policy does not exist', async () => {
    renderApp('/cleanup?policy=gone');
    expect(await screen.findByText(/There is no policy named/)).toBeInTheDocument();
  });
});
