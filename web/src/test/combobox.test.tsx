import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState } from 'react';
import { describe, expect, it, vi } from 'vitest';
import { Combobox } from '../components/Combobox';
import { matchSuggestions } from '../lib/suggestions';
import { renderApp } from './render';

function Harness({ onSubmit }: { onSubmit: (v: string) => void }) {
  const [v, setV] = useState('');
  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        onSubmit(v);
      }}
    >
      <label htmlFor="c">Action</label>
      <Combobox id="c" value={v} onChange={setV} options={['repository.create', 'repository.delete', 'user.create']} />
    </form>
  );
}

describe('combobox', () => {
  it('orders prefix matches first and hides a lone exact match', () => {
    expect(matchSuggestions(['user.create', 'repository.create'], 'create')).toEqual(['user.create', 'repository.create']);
    expect(matchSuggestions(['user.create', 'repository.create'], 'rep')).toEqual(['repository.create']);
    expect(matchSuggestions(['user.create'], 'user.create')).toEqual([]);
  });

  it('suggests, moves with the keyboard, picks with Enter, closes with Escape, and keeps free text', async () => {
    const user = userEvent.setup();
    const submitted = vi.fn();
    render(<Harness onSubmit={submitted} />);
    const box = screen.getByRole('combobox', { name: 'Action' });
    expect(box).toHaveAttribute('aria-autocomplete', 'list');
    await user.type(box, 'repo');
    expect(box).toHaveAttribute('aria-expanded', 'true');
    expect(screen.getAllByRole('option').map((o) => o.textContent)).toEqual(['repository.create', 'repository.delete']);
    await user.keyboard('{ArrowDown}{ArrowDown}');
    expect(box).toHaveAttribute('aria-activedescendant', screen.getAllByRole('option')[1]!.id);
    await user.keyboard('{Enter}');
    expect(box).toHaveValue('repository.delete');
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument();
    expect(submitted).not.toHaveBeenCalled();

    await user.clear(box);
    await user.type(box, 'user');
    expect(screen.getByRole('listbox')).toBeInTheDocument();
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument();
    // Free text: Enter with nothing highlighted submits what was typed.
    await user.type(box, '.*{Enter}');
    expect(submitted).toHaveBeenCalledWith('user.*');
  });

  it('replaces the native suggestion list on the audit log filter', async () => {
    const user = userEvent.setup();
    renderApp('/audit');
    const action = await screen.findByRole('combobox', { name: 'Action' });
    expect(document.querySelector('datalist')).toBeNull();
    await user.type(action, 'repository.');
    expect(screen.getAllByRole('option').length).toBeGreaterThan(0);
    await user.click(screen.getAllByRole('option')[0]!);
    expect((action as HTMLInputElement).value).toMatch(/^repository\./);
  });
});
