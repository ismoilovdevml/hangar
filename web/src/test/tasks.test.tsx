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

describe('scheduled tasks', () => {
  it('shows the running banner and every task with schedule, last and next run', async () => {
    renderApp('/tasks');
    const banner = await screen.findByRole('region', { name: 'Running now: Rebuild search index · maven-all' });
    expect(within(banner).getByText(/^64%/)).toBeInTheDocument();
    expect(within(banner).getByRole('meter', { name: /Progress of Rebuild search index/ })).toHaveAttribute('aria-valuenow', '64');
    expect(within(banner).getByRole('button', { name: 'Stop Rebuild search index · maven-all' })).toBeInTheDocument();

    const table = screen.getByRole('table', { name: 'Scheduled tasks' });
    const backup = within(table).getByRole('link', { name: 'Backup database' }).closest('tr')!;
    expect(within(backup).getByText('Every day at 01:00 UTC')).toBeInTheDocument();
    expect(within(backup).getByText('0 1 * * *')).toBeInTheDocument();
    expect(within(backup).getByText('2.4 GB · 6 min')).toBeInTheDocument();
    const replicate = within(table).getByRole('link', { name: 'Replicate to us-east' }).closest('tr')!;
    expect(within(replicate).getByText(/Failed: Retrying · 2 failed/)).toBeInTheDocument();
    const reindex = within(table).getByRole('link', { name: 'Rebuild search index · maven-all' }).closest('tr')!;
    expect(within(reindex).getByText('Running · 64%')).toBeInTheDocument();
    expect(screen.getByText(`Showing ${opsMockState.tasks.length} of ${opsMockState.tasks.length}`)).toBeInTheDocument();
  });

  it('keeps type, status and name filters in the URL', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/tasks');
    const table = await screen.findByRole('table', { name: 'Scheduled tasks' });
    await within(table).findByText('Backup database');

    await user.selectOptions(screen.getByLabelText('Status'), 'failed');
    await waitFor(() => expect(search(history).get('status')).toBe('failed'));
    expect(within(table).getAllByRole('row')).toHaveLength(2);
    expect(within(table).getByText('Replicate to us-east')).toBeInTheDocument();

    await user.selectOptions(screen.getByLabelText('Status'), '');
    await user.selectOptions(screen.getByLabelText('Type'), 'db_backup');
    await waitFor(() => expect(search(history).get('type')).toBe('db_backup'));
    expect(within(table).getAllByRole('row')).toHaveLength(2);

    await user.selectOptions(screen.getByLabelText('Type'), '');
    await user.type(screen.getByLabelText('Filter tasks by name'), 'active directory');
    await waitFor(() => expect(search(history).get('q')).toBe('active directory'));
    await waitFor(() => expect(within(table).getAllByRole('row')).toHaveLength(2));
    expect(within(table).getByText('Sync users from Active Directory')).toBeInTheDocument();
  });

  it('runs a task and follows its progress over the event stream', async () => {
    opsMockState.stepMs = 40;
    const user = userEvent.setup();
    renderApp('/tasks');
    const table = await screen.findByRole('table', { name: 'Scheduled tasks' });
    await user.click(await within(table).findByRole('button', { name: 'Run Backup database now' }));
    const banner = await screen.findByRole('region', { name: 'Running now: Backup database' });
    // Progress arrives as task.progress events; the run then finishes and leaves the banner.
    await waitFor(() => expect(within(banner).getByRole('meter')).not.toHaveAttribute('aria-valuenow', '0'), { timeout: 3000 });
    await waitFor(() => expect(screen.queryByRole('region', { name: 'Running now: Backup database' })).not.toBeInTheDocument(), {
      timeout: 4000,
    });
    const row = within(table).getByRole('link', { name: 'Backup database' }).closest('tr')!;
    expect(within(row).getByText(/^Completed/)).toBeInTheDocument();
  });

  it('asks before running a task that deletes data, and before stopping', async () => {
    const user = userEvent.setup();
    renderApp('/tasks');
    const table = await screen.findByRole('table', { name: 'Scheduled tasks' });
    await user.click(await within(table).findByRole('button', { name: 'Run Docker garbage collection now' }));
    const dialog = screen.getByRole('alertdialog', { name: 'Run Docker garbage collection now?' });
    await user.keyboard('{Escape}');
    expect(dialog).not.toBeInTheDocument();

    await user.click(within(table).getByRole('button', { name: 'Stop Rebuild search index · maven-all' }));
    await user.click(within(screen.getByRole('alertdialog')).getByRole('button', { name: 'Stop task' }));
    await waitFor(() => expect(screen.queryByRole('region', { name: /Running now/ })).not.toBeInTheDocument());
    const row = within(table).getByRole('link', { name: 'Rebuild search index · maven-all' }).closest('tr')!;
    await waitFor(() => expect(within(row).getByText(/Cancelled: Stopped by/)).toBeInTheDocument());
  });

  it('hides run and stop without the tasks write privilege', async () => {
    server.use(
      http.get('*/api/v1/auth/session', () =>
        mockState.signedIn ? HttpResponse.json({ ...session, privileges: ['system:tasks:read'] }) : HttpResponse.json({}, { status: 401 }),
      ),
    );
    renderApp('/tasks');
    await screen.findByRole('table', { name: 'Scheduled tasks' });
    await screen.findByText(/needs the tasks write privilege/);
    expect(screen.queryByRole('button', { name: /Run .* now/ })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /^Stop/ })).not.toBeInTheDocument();
  });
});

describe('task detail', () => {
  it('shows the live run with progress and its log', async () => {
    renderApp('/tasks/t_reindex');
    expect(await screen.findByRole('heading', { level: 1, name: 'Rebuild search index · maven-all' })).toBeInTheDocument();
    const detail = await screen.findByRole('region', { name: 'Run t_reindex_r1' });
    expect(within(detail).getByText('Running')).toBeInTheDocument();
    expect(within(detail).getByRole('meter', { name: 'Run progress' })).toHaveAttribute('aria-valuenow', '64');
    const log = within(detail).getByRole('list', { name: 'Log of run t_reindex_r1' });
    expect(await within(log).findByText('maven-central: 12 components without metadata, skipped')).toBeInTheDocument();
    expect(within(log).getByText('warn')).toBeInTheDocument();
  });

  it('pages through run history and selects a run', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/tasks/t_rescan');
    const runs = await screen.findByRole('table', { name: 'Runs, newest first' });
    await waitFor(() => expect(within(runs).getAllByRole('row')).toHaveLength(21));
    await user.click(screen.getByRole('button', { name: 'Next' }));
    await waitFor(() => expect(search(history).get('cursor')).toBe('o20'));
    await waitFor(() => expect(within(runs).getAllByRole('row')).toHaveLength(7));
    const failed = within(runs).getAllByText('Failed')[0]!.closest('tr')!;
    await user.click(within(failed).getByRole('link'));
    await waitFor(() => expect(search(history).get('run')).toBeTruthy());
    const detail = screen.getByRole('region', { name: `Run ${search(history).get('run')}` });
    expect(await within(detail).findByText('Upstream timed out after 30 s', { selector: '.run-log__msg' })).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Previous' }));
    await waitFor(() => expect(search(history).get('cursor')).toBeNull());
  });

  it('says when the task does not exist', async () => {
    renderApp('/tasks/t_missing');
    expect(await screen.findByRole('heading', { level: 1, name: 'Task not found' })).toBeInTheDocument();
  });
});
