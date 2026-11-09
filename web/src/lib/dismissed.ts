import { useCallback, useMemo, useState } from 'react';

const KEY = 'hangar.dismissed';

function read(): string[] {
  try {
    const v: unknown = JSON.parse(window.localStorage.getItem(KEY) ?? '[]');
    return Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string') : [];
  } catch {
    return [];
  }
}

/**
 * Reminders this browser was told to stop showing. Storage may be missing or
 * blocked (private windows): dismissing then lasts until the page reloads.
 */
export function useDismissed(): [ReadonlySet<string>, (id: string) => void] {
  const [ids, setIds] = useState<string[]>(read);
  const dismiss = useCallback((id: string) => {
    setIds((prev) => {
      const next = prev.includes(id) ? prev : [...prev, id];
      try {
        window.localStorage.setItem(KEY, JSON.stringify(next));
      } catch {
        // not persisted; still hidden for now
      }
      return next;
    });
  }, []);
  const set = useMemo(() => new Set(ids), [ids]);
  return [set, dismiss];
}
