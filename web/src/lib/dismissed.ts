import { useCallback, useEffect, useMemo, useState } from 'react';

const KEY = 'hangar.dismissed';
/** Tells every other user of the hook in this page (To do and the bell) what was dismissed. */
const EVENT = 'hangar:dismissed';

function read(): string[] {
  try {
    const v: unknown = JSON.parse(window.localStorage.getItem(KEY) ?? '[]');
    return Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string') : [];
  } catch {
    return [];
  }
}

const withId = (prev: string[], id: string) => (prev.includes(id) ? prev : [...prev, id]);

/**
 * Reminders this browser was told to stop showing. Storage may be missing or
 * blocked (private windows): dismissing then lasts until the page reloads.
 */
export function useDismissed(): [ReadonlySet<string>, (id: string) => void] {
  const [ids, setIds] = useState<string[]>(read);
  useEffect(() => {
    const on = (e: Event) => {
      const id = (e as CustomEvent<string>).detail;
      if (typeof id === 'string') setIds((prev) => withId(prev, id));
    };
    window.addEventListener(EVENT, on);
    return () => window.removeEventListener(EVENT, on);
  }, []);
  const dismiss = useCallback((id: string) => {
    try {
      window.localStorage.setItem(KEY, JSON.stringify(withId(read(), id)));
    } catch {
      // not persisted; still hidden for now
    }
    window.dispatchEvent(new CustomEvent(EVENT, { detail: id }));
  }, []);
  const set = useMemo(() => new Set(ids), [ids]);
  return [set, dismiss];
}
