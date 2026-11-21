import { fireEvent, screen, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it, vi } from 'vitest';
import { repositories } from '../mocks/data';
import { server } from '../mocks/node';
import { flattenTree } from '../pages/browse/treeModel';
import { renderApp } from './render';

// Counts how often the tree's rows are rebuilt.
vi.mock('../pages/browse/treeModel', async (load) => {
  const actual = await load<typeof import('../pages/browse/treeModel')>();
  return { ...actual, flattenTree: vi.fn(actual.flattenTree) };
});

const ROW = 34;

describe('package browser: a long tree', () => {
  it('renders only the rows in view and does not rebuild the tree while it scrolls', async () => {
    const template = repositories()[1]!;
    const many = Array.from({ length: 300 }, (_, i) => ({ ...template, id: `r_${i}`, name: `repo-${String(i).padStart(3, '0')}` }));
    server.use(
      http.get('*/api/v1/repositories', () =>
        HttpResponse.json({ items: many, next_cursor: null, counts: { all: many.length, hosted: many.length, proxy: 0, group: 0 } }),
      ),
    );
    renderApp('/browse');
    const tree = await screen.findByRole('tree', { name: 'Repositories' });
    await within(tree).findByRole('treeitem', { name: /^repo-000/ });
    // Virtualised: a window of rows, not all 300.
    expect(within(tree).getAllByRole('treeitem').length).toBeLessThan(60);
    expect(within(tree).queryByRole('treeitem', { name: /^repo-200/ })).toBeNull();

    const builds = vi.mocked(flattenTree).mock.calls.length;
    // Pixel by pixel through the first rows: the rows are built once, the window moves by whole rows.
    for (let top = 1; top <= 3 * ROW; top++) fireEvent.scroll(tree, { target: { scrollTop: top } });
    expect(vi.mocked(flattenTree).mock.calls.length).toBe(builds);

    // Far down: the window follows, still without a rebuild.
    fireEvent.scroll(tree, { target: { scrollTop: 200 * ROW } });
    expect(await within(tree).findByRole('treeitem', { name: /^repo-200/ })).toBeInTheDocument();
    expect(within(tree).queryByRole('treeitem', { name: /^repo-000/ })).toBeNull();
    expect(vi.mocked(flattenTree).mock.calls.length).toBe(builds);
  });
});
