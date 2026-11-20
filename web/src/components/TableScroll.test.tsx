import { render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { TableScroll } from './TableScroll';

/** The table is 300 px wide, 150 without level 3 and 90 without levels 2 and 3, in a 100 px box. */
let widths = { all: 300, hide3: 150, hide2: 90 };

function table(rows: string[]) {
  return (
    <table>
      <tbody>
        {rows.map((r) => (
          <tr key={r}>
            <td>{r}</td>
            <td data-priority="2">two</td>
            <td data-priority="3">three</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

describe('TableScroll', () => {
  afterEach(() => {
    vi.restoreAllMocks();
    widths = { all: 300, hide3: 150, hide2: 90 };
  });

  it('hides columns until the table fits, and fits again only when the rows change', async () => {
    vi.spyOn(HTMLElement.prototype, 'clientWidth', 'get').mockReturnValue(100);
    const scrollWidth = vi.spyOn(HTMLElement.prototype, 'scrollWidth', 'get').mockImplementation(function (this: HTMLElement) {
      const hide = this.dataset.hide;
      return hide === '2' ? widths.hide2 : hide === '3' ? widths.hide3 : widths.all;
    });
    const { rerender } = render(<TableScroll label="Rows">{table(['a'])}</TableScroll>);
    const scroller = screen.getByRole('region', { name: 'Rows' });
    expect(scroller.dataset.hide).toBe('2');

    // A render that changes nothing in the table measures nothing.
    const reads = scrollWidth.mock.calls.length;
    rerender(<TableScroll label="Rows">{table(['a'])}</TableScroll>);
    await Promise.resolve();
    expect(scrollWidth.mock.calls.length).toBe(reads);

    // New rows that fit as they are: every column shows again.
    widths = { all: 90, hide3: 90, hide2: 90 };
    rerender(<TableScroll label="Rows">{table(['a', 'b'])}</TableScroll>);
    await waitFor(() => expect(scroller.dataset.hide).toBeUndefined());
  });
});
