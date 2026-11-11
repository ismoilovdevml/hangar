import type { ReactNode } from 'react';
import { IconArrowDown, IconArrowUp } from './icons';
import { TableScroll, type ColumnPriority } from './TableScroll';

export interface Column<T> {
  key: string;
  header: ReactNode;
  cell: (row: T) => ReactNode;
  align?: 'left' | 'right';
  className?: string;
  /** The column can order the table: its header becomes a button. */
  sortable?: boolean;
  /** 2 or 3: the column gives way (3 first) before the table scrolls sideways; leave out for the name, status and actions. */
  priority?: ColumnPriority;
  /** Label of the value in a phone card, when the header is not plain text. */
  label?: string;
}

/** The column the table is ordered by. */
export interface TableSort {
  key: string;
  desc: boolean;
}

interface DataTableProps<T> {
  caption: string;
  columns: Column<T>[];
  rows: T[];
  rowKey: (row: T) => string;
  minWidth?: number;
  empty?: ReactNode;
  busy?: boolean;
  sort?: TableSort;
  /** Called with the key of a sortable column whose header was activated. */
  onSort?: (key: string) => void;
  /** On phones each row becomes a card (default); false keeps the table, which then scrolls. */
  stack?: boolean;
}

const cardLabel = <T,>(c: Column<T>) => c.label ?? (typeof c.header === 'string' ? c.header : '');

/**
 * A table that lives inside a Panel. When it does not fit, columns with a
 * priority give way first, then it scrolls sideways; on phones its rows
 * become cards.
 */
export function DataTable<T>({ caption, columns, rows, rowKey, minWidth = 640, empty, busy, sort, onSort, stack = true }: DataTableProps<T>) {
  return (
    <TableScroll label={`${caption} table`}>
      <table className={stack ? 'data-table data-table--stack' : 'data-table'} style={{ minWidth }} aria-busy={busy || undefined}>
        <caption className="visually-hidden">{caption}</caption>
        <thead>
          <tr>
            {columns.map((c) => {
              const active = sort?.key === c.key;
              return (
                <th
                  key={c.key}
                  scope="col"
                  className={[c.align === 'right' ? 'is-right' : '', c.className ?? ''].join(' ')}
                  data-priority={c.priority}
                  aria-sort={c.sortable && onSort ? (active ? (sort.desc ? 'descending' : 'ascending') : 'none') : undefined}
                >
                  {c.sortable && onSort ? (
                    <button type="button" className={active ? 'th-sort is-active' : 'th-sort'} onClick={() => onSort(c.key)}>
                      {c.header}
                      <span className="th-sort__mark" aria-hidden="true">
                        {active && (sort.desc ? <IconArrowDown size={13} /> : <IconArrowUp size={13} />)}
                      </span>
                    </button>
                  ) : (
                    c.header
                  )}
                </th>
              );
            })}
          </tr>
        </thead>
        <tbody>
          {rows.length === 0 ? (
            <tr>
              <td colSpan={columns.length} className="data-table__empty">
                {empty ?? 'Nothing here yet.'}
              </td>
            </tr>
          ) : (
            rows.map((r) => (
              <tr key={rowKey(r)}>
                {columns.map((c) => (
                  <td key={c.key} className={[c.align === 'right' ? 'is-right' : '', c.className ?? ''].join(' ')} data-priority={c.priority} data-label={cardLabel(c)}>
                    {c.cell(r)}
                  </td>
                ))}
              </tr>
            ))
          )}
        </tbody>
      </table>
    </TableScroll>
  );
}
