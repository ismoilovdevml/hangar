import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { renderApp } from './render';

describe('login', () => {
  it('shows providers and signs in to the overview', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/login');
    expect(await screen.findByRole('link', { name: 'Continue with company SSO' })).toHaveAttribute('href', '/api/v1/auth/oidc/start');
    expect(await screen.findByText('Sign in to acme-platform registry')).toBeInTheDocument();
    await user.type(screen.getByLabelText('Username'), 'd.ismoilov');
    await user.type(screen.getByLabelText('Password'), 'correct-horse');
    await user.click(screen.getByRole('button', { name: 'Sign in' }));
    await waitFor(() => expect(history.location.pathname).toBe('/'));
  });

  it('explains a wrong password', async () => {
    const user = userEvent.setup();
    renderApp('/login');
    await user.type(await screen.findByLabelText('Username'), 'd.ismoilov');
    await user.type(screen.getByLabelText('Password'), 'wrong');
    await user.click(screen.getByRole('button', { name: 'Sign in' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Wrong username or password.');
  });
});
