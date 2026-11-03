import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { renderApp } from './render';

describe('command palette', () => {
  it('opens with ⌘K, filters, and navigates with the keyboard', async () => {
    const user = userEvent.setup();
    const { history } = renderApp('/');
    await screen.findByRole('navigation', { name: 'Primary' });

    await user.keyboard('{Meta>}k{/Meta}');
    const dialog = await screen.findByRole('dialog', { name: 'Command palette' });
    const input = within(dialog).getByRole('combobox');
    expect(input).toHaveFocus();

    await user.type(input, 'cluster');
    const options = within(dialog).getAllByRole('option');
    expect(options[0]).toHaveTextContent('Search packages for “cluster”');
    await user.keyboard('{ArrowDown}');
    expect(within(dialog).getByRole('option', { name: 'Cluster & replication' })).toHaveAttribute('aria-selected', 'true');
    await user.keyboard('{Enter}');

    await waitFor(() => expect(history.location.pathname).toBe('/cluster'));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('opens with Ctrl+K and the search button, and closes with Escape', async () => {
    const user = userEvent.setup();
    renderApp('/');
    const trigger = await screen.findByRole('button', { name: /Search packages, digests, CVEs/ });

    await user.keyboard('{Control>}k{/Control}');
    expect(await screen.findByRole('dialog')).toBeInTheDocument();
    await user.keyboard('{Escape}');
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());

    await user.click(trigger);
    expect(await screen.findByRole('dialog')).toBeInTheDocument();
    await user.keyboard('{Escape}');
    await waitFor(() => expect(trigger).toHaveFocus());
  });
});
