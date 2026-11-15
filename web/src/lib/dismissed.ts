import { useCallback, useEffect, useMemo, useState } from 'react';

const KEY = 'hangar.dismissed';
/** Tells every other user of the hook in this page (To do and the bell) what was dismissed or shown again. */
const EVENT = 'hangar:dismissed';

interface Change {
  add?: string;
  remove?: readonly string[];
}

function read(): string[] {
  try {
    const v: unknown = JSON.parse(window.localStorage.getItem(KEY) ?? '[]');
    return Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string') : [];
  } catch {
    return [];
  }
}

function apply(prev: string[], c: Change): string[] {
  let next = prev;
  if (c.add && !next.includes(c.add)) next = [...next, c.add];
  if (c.remove?.length) next = next.filter((id) => !c.remove!.includes(id));
  return next;
}

function change(c: Change) {
  try {
    window.localStorage.setItem(KEY, JSON.stringify(apply(read(), c)));
  } catch {
    // not persisted; the change holds until the page reloads
  }
  window.dispatchEvent(new CustomEvent<Change>(EVENT, { detail: c }));
}

/**
 * Reminders this browser was told to stop showing. Storage may be missing or
 * blocked (private windows): dismissing then lasts until the page reloads.
 * Returns the hidden ids, dismiss, and restore (shows reminders again).
 */
export function useDismissed(): [ReadonlySet<string>, (id: string) => void, (ids: readonly string[]) => void] {
  const [ids, setIds] = useState<string[]>(read);
  useEffect(() => {
    const on = (e: Event) => {
      const c = (e as CustomEvent<Change>).detail;
      if (c && typeof c === 'object') setIds((prev) => apply(prev, c));
    };
    window.addEventListener(EVENT, on);
    return () => window.removeEventListener(EVENT, on);
  }, []);
  const dismiss = useCallback((id: string) => change({ add: id }), []);
  const restore = useCallback((back: readonly string[]) => change({ remove: back }), []);
  const set = useMemo(() => new Set(ids), [ids]);
  return [set, dismiss, restore];
}
