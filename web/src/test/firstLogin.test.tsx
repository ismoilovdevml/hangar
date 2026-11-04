import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { passwordChecks } from '../lib/passwordPolicy';
import { accessState } from '../mocks/access';
import { mockState } from '../mocks/handlers';
import { renderApp } from './render';

const INITIAL = 'Gk7mPq2RtW9xZb4NcV8sHd3F';

type User = ReturnType<typeof userEvent.setup>;

/** Signs in as the generated first administrator from /login?redirect=... */
async function signInAsInitialAdmin(user: User, path = '/login') {
  const utils = renderApp(path);
  await user.type(await screen.findByLabelText('Username'), 'admin');
  await user.type(screen.getByLabelText('Password'), INITIAL);
  await user.click(screen.getByRole('button', { name: 'Sign in' }));
  await screen.findByRole('heading', { name: 'Set a new password' });
  return utils;
}

const checkItem = (name: RegExp) => within(screen.getByRole('list', { name: 'Password requirements' })).getByText(name).closest('li')!;

describe('first sign-in password change', () => {
  it('routes a user who must change their password to the change page, and nowhere else', async () => {
    const user = userEvent.setup();
    const { history } = await signInAsInitialAdmin(user);
    expect(history.location.pathname).toBe('/change-password');
    expect(screen.getByText('admin')).toBeInTheDocument();
    // No shell, no way around it.
    expect(screen.queryByRole('navigation', { name: /main/i })).not.toBeInTheDocument();
    history.push('/repositories');
    await waitFor(() => expect(history.location.pathname).toBe('/change-password'));
    expect(new URLSearchParams(history.location.search).get('redirect')).toBe('/repositories');
    expect(mockState.passwordChangeRequired).toBe(true);
  });

  it('checks the policy live and validates before sending', async () => {
    const user = userEvent.setup();
    await signInAsInitialAdmin(user);
    await user.click(screen.getByRole('button', { name: 'Set password and continue' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Enter your current password.');

    await user.type(screen.getByLabelText('Current password'), INITIAL);
    await user.type(screen.getByLabelText('New password'), 'admin-short');
    expect(checkItem(/At least 12 characters/)).not.toHaveClass('is-met');
    expect(checkItem(/Does not contain your username/)).not.toHaveClass('is-met');
    await user.click(screen.getByRole('button', { name: 'Set password and continue' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('does not meet every requirement');
    expect(screen.getByLabelText('New password')).toHaveAttribute('aria-invalid', 'true');

    await user.clear(screen.getByLabelText('New password'));
    await user.type(screen.getByLabelText('New password'), INITIAL);
    expect(checkItem(/Different from the current password/)).not.toHaveClass('is-met');

    await user.clear(screen.getByLabelText('New password'));
    await user.type(screen.getByLabelText('New password'), 'a long private passphrase');
    await user.type(screen.getByLabelText('Confirm new password'), 'a long private passphrasE');
    expect(checkItem(/Both new password fields match/)).not.toHaveClass('is-met');
    expect(checkItem(/At least 12 characters/)).toHaveClass('is-met');

    // Show passwords reveals all three fields.
    expect(screen.getByLabelText('New password')).toHaveAttribute('type', 'password');
    await user.click(screen.getByLabelText('Show passwords'));
    expect(screen.getByLabelText('Current password')).toHaveAttribute('type', 'text');
    expect(screen.getByLabelText('New password')).toHaveAttribute('type', 'text');
    expect(screen.getByLabelText('Confirm new password')).toHaveAttribute('type', 'text');
  });

  it('explains a wrong current password', async () => {
    const user = userEvent.setup();
    await signInAsInitialAdmin(user);
    await user.type(screen.getByLabelText('Current password'), 'not the one I was given');
    await user.type(screen.getByLabelText('New password'), 'a long private passphrase');
    await user.type(screen.getByLabelText('Confirm new password'), 'a long private passphrase');
    await user.click(screen.getByRole('button', { name: 'Set password and continue' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('The current password is wrong.');
    expect(mockState.passwordChangeRequired).toBe(true);
  });

  it('continues to the page that was asked for after the change', async () => {
    const user = userEvent.setup();
    mockState.signedIn = false;
    const { history } = renderApp('/repositories');
    await waitFor(() => expect(history.location.pathname).toBe('/login'));
    await user.type(await screen.findByLabelText('Username'), 'admin');
    await user.type(screen.getByLabelText('Password'), INITIAL);
    await user.click(screen.getByRole('button', { name: 'Sign in' }));
    await screen.findByRole('heading', { name: 'Set a new password' });
    expect(new URLSearchParams(history.location.search).get('redirect')).toBe('/repositories');

    await user.type(screen.getByLabelText('Current password'), INITIAL);
    await user.type(screen.getByLabelText('New password'), 'a long private passphrase');
    await user.type(screen.getByLabelText('Confirm new password'), 'a long private passphrase');
    await user.click(screen.getByRole('button', { name: 'Set password and continue' }));
    await waitFor(() => expect(history.location.pathname).toBe('/repositories'));
    expect(mockState.passwordChangeRequired).toBe(false);
    expect(await screen.findByRole('heading', { name: 'Repositories', level: 1 })).toBeInTheDocument();
  });

  it('lets the user sign out instead', async () => {
    const user = userEvent.setup();
    const { history } = await signInAsInitialAdmin(user);
    await user.click(screen.getByRole('button', { name: 'Sign out' }));
    await waitFor(() => expect(history.location.pathname).toBe('/login'));
    expect(mockState.signedIn).toBe(false);
  });

  it('sends an open tab to the change page when the server starts refusing', async () => {
    const { history } = renderApp('/repositories');
    await screen.findByRole('heading', { name: 'Repositories', level: 1 });
    // An administrator required a change meanwhile: every call is now refused.
    mockState.passwordChangeRequired = true;
    history.push('/blob-stores');
    await waitFor(() => expect(history.location.pathname).toBe('/change-password'));
    expect(await screen.findByRole('heading', { name: 'Set a new password' })).toBeInTheDocument();
  });
});

describe('password policy checks', () => {
  it('mirrors the server rules', () => {
    const policy = { min_length: 12, max_length: 20, min_classes: 3, allow_username: false };
    const byId = (pw: string, confirm = pw) => Object.fromEntries(passwordChecks(policy, 'Admin', 'old', pw, confirm).map((c) => [c.id, c.met]));
    expect(byId('abcdefghijkl')).toMatchObject({ length: true, classes: false, username: true, differs: true, match: true });
    expect(byId('Abcdefghijk1')).toMatchObject({ classes: true });
    expect(byId('xxadminxxxxX1')).toMatchObject({ username: false });
    expect(byId('Abcdefghijk1Abcdefghijk1')).toMatchObject({ length: false });
    expect(byId('Abcdefghijk1', 'other')).toMatchObject({ match: false });
    expect(passwordChecks({ ...policy, min_classes: 0, allow_username: true }, 'admin', '', 'x', 'x').map((c) => c.id)).toEqual(['length', 'differs', 'match']);
  });
});

describe('requiring a password change', () => {
  it('shows the badge and lets an administrator require a change', async () => {
    const user = userEvent.setup();
    renderApp('/access');
    const table = await screen.findByRole('table', { name: 'Members' });
    const flagged = (await within(table).findByRole('button', { name: 'Open break-glass-1' })).closest('tr')!;
    expect(within(flagged).getByText('Must change password')).toBeInTheDocument();

    await user.click(within(table).getByRole('button', { name: 'Open Bekzod Yusupov' }));
    const drawer = await screen.findByRole('dialog', { name: 'Bekzod Yusupov' });
    await user.click(within(drawer).getByRole('button', { name: 'Require password change' }));
    const confirm = await screen.findByRole('dialog', { name: 'Require a password change at next sign-in?' });
    await user.click(within(confirm).getByRole('button', { name: 'Require password change' }));
    expect(await within(drawer).findByText('They must set a new password at their next sign-in.')).toBeInTheDocument();
    expect(accessState.users.find((u) => u.id === 'u_09')?.must_change_password).toBe(true);
    expect(accessState.audit.some((e) => e.action === 'user.password.require_change' && e.target === 'user:b.yusupov')).toBe(true);
    expect(await within(drawer).findByText('Must change password')).toBeInTheDocument();
    await waitFor(() => expect(within(drawer).queryByRole('button', { name: 'Require password change' })).not.toBeInTheDocument());
  });

  it('is not offered for single sign-on users or yourself', async () => {
    renderApp('/access?user=u_02');
    const drawer = await screen.findByRole('dialog', { name: 'Jasur Karimov' });
    expect(within(drawer).queryByRole('button', { name: 'Require password change' })).not.toBeInTheDocument();
  });
});
