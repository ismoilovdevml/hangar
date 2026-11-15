import { useCallback, useEffect, useRef } from 'react';
import { useBlocker } from '@tanstack/react-router';

interface Place {
  pathname: string;
  search: unknown;
}

interface GuardOptions {
  /**
   * Whether moving from current to next leaves the form. By default only a
   * new path does; pages that keep the edited object in the query string
   * (?policy=…) pass pathOrSearchChanges.
   */
  leaves?: (current: Place, next: Place) => boolean;
}

const newPath = (current: Place, next: Place) => current.pathname !== next.pathname;

/** Leaving when the path or the query string changes. */
export const pathOrSearchChanges = (current: Place, next: Place) =>
  newPath(current, next) || JSON.stringify(current.search) !== JSON.stringify(next.search);

/**
 * Asks before in-app navigation, and lets the browser ask before a reload or
 * closing the tab, while the form has unsaved changes. Render
 * <UnsavedChangesDialog blocker={guard.blocker} /> with it. Call
 * allowNextNavigation() just before navigating away on purpose (after a
 * save or a delete), when the form may still look dirty.
 */
export function useUnsavedChangesGuard(dirty: boolean, { leaves = newPath }: GuardOptions = {}) {
  const dirtyRef = useRef(dirty);
  const leavesRef = useRef(leaves);
  useEffect(() => {
    dirtyRef.current = dirty;
    leavesRef.current = leaves;
  }, [dirty, leaves]);
  const allowed = useRef(false);

  const blocker = useBlocker({
    shouldBlockFn: ({ current, next }) => {
      if (allowed.current) {
        allowed.current = false;
        return false;
      }
      return dirtyRef.current && leavesRef.current(current, next);
    },
    enableBeforeUnload: () => dirtyRef.current && !allowed.current,
    withResolver: true,
  });
  const allowNextNavigation = useCallback(() => {
    allowed.current = true;
  }, []);
  return { blocker, allowNextNavigation };
}

export type UnsavedChangesBlocker = ReturnType<typeof useUnsavedChangesGuard>['blocker'];
