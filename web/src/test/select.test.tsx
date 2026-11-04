import { useState } from 'react';
import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { Select, type SelectOption } from '../components/Select';

const FRUIT: SelectOption[] = [
  { value: 'apple', label: 'Apple' },
  { value: 'banana', label: 'Banana', disabled: true },
  { value: 'blueberry', label: 'Blueberry' },
  { value: 'cherry', label: 'Cherry', hint: 'stone fruit' },
];

function Harness({ options = FRUIT, initial = 'apple', onSubmit }: { options?: SelectOption[]; initial?: string; onSubmit?: (v: FormDataEntryValue | null) => void }) {
  const [value, setValue] = useState(initial);
  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        onSubmit?.(new FormData(e.currentTarget).get('fruit'));
      }}
    >
      <label htmlFor="fruit">Fruit</label>
      <Select id="fruit" name="fruit" value={value} options={options} onChange={setValue} />
      <button type="submit">Save</button>
    </form>
  );
}

describe('Select', () => {
  it('is a labelled combobox that opens a listbox with the selected option marked', async () => {
    const user = userEvent.setup();
    render(<Harness />);
    const box = screen.getByRole('combobox', { name: 'Fruit' });
    expect(box).toHaveAttribute('aria-expanded', 'false');
    expect(box).toHaveTextContent('Apple');
    await user.click(box);
    expect(box).toHaveAttribute('aria-expanded', 'true');
    const list = screen.getByRole('listbox');
    expect(box).toHaveAttribute('aria-controls', list.id);
    expect(within(list).getByRole('option', { name: 'Apple' })).toHaveAttribute('aria-selected', 'true');
    expect(within(list).getByRole('option', { name: 'Banana' })).toHaveAttribute('aria-disabled', 'true');
  });

  it('moves with the arrow keys past disabled options, picks with Enter and closes on Escape', async () => {
    const user = userEvent.setup();
    render(<Harness />);
    const box = screen.getByRole('combobox', { name: 'Fruit' });
    box.focus();
    await user.keyboard('{ArrowDown}');
    expect(screen.getByRole('listbox')).toBeInTheDocument();
    await user.keyboard('{ArrowDown}');
    // Banana is disabled, so the next stop is Blueberry.
    expect(box).toHaveAttribute('aria-activedescendant', screen.getByRole('option', { name: 'Blueberry' }).id);
    await user.keyboard('{Enter}');
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument();
    expect(box).toHaveTextContent('Blueberry');
    expect(box).toHaveFocus();

    await user.keyboard('{End}');
    expect(box).toHaveAttribute('aria-activedescendant', screen.getByRole('option', { name: /Cherry/ }).id);
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument();
    expect(box).toHaveTextContent('Blueberry');
  });

  it('jumps to an option by typing its first letters', async () => {
    const user = userEvent.setup();
    render(<Harness />);
    const box = screen.getByRole('combobox', { name: 'Fruit' });
    box.focus();
    await user.keyboard('c');
    expect(box).toHaveAttribute('aria-activedescendant', screen.getByRole('option', { name: /Cherry/ }).id);
    await user.keyboard('{Enter}');
    expect(box).toHaveTextContent('Cherry');
  });

  it('submits its value with the form', async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    render(<Harness onSubmit={onSubmit} />);
    await user.click(screen.getByRole('combobox', { name: 'Fruit' }));
    await user.click(screen.getByRole('option', { name: /Cherry/ }));
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(onSubmit).toHaveBeenCalledWith('cherry');
  });

  it('filters long lists with a search box', async () => {
    const user = userEvent.setup();
    const many = Array.from({ length: 14 }, (_, i) => ({ value: `repo-${i}`, label: `repo-${i}` }));
    render(<Harness options={many} initial="repo-0" />);
    await user.click(screen.getByRole('combobox', { name: 'Fruit' }));
    const search = screen.getByRole('searchbox', { name: 'Filter options' });
    expect(search).toHaveFocus();
    await user.type(search, '12');
    expect(screen.getAllByRole('option')).toHaveLength(1);
    await user.keyboard('{Enter}');
    expect(screen.getByRole('combobox', { name: 'Fruit' })).toHaveTextContent('repo-12');
    await user.click(screen.getByRole('combobox', { name: 'Fruit' }));
    await user.type(screen.getByRole('searchbox', { name: 'Filter options' }), 'nothing');
    expect(screen.getByText(/No matches/)).toBeInTheDocument();
  });

  it('closes on a click outside without changing the value', async () => {
    const user = userEvent.setup();
    render(<Harness />);
    await user.click(screen.getByRole('combobox', { name: 'Fruit' }));
    await user.click(document.body);
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument();
    expect(screen.getByRole('combobox', { name: 'Fruit' })).toHaveTextContent('Apple');
  });
});
