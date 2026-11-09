import { useSyncExternalStore } from 'react';

/** Whether `query` matches now; false where the browser cannot tell (tests, server rendering). */
export function useMediaQuery(query: string): boolean {
  return useSyncExternalStore(
    (onChange) => {
      if (typeof window === 'undefined' || !window.matchMedia) return () => {};
      const list = window.matchMedia(query);
      list.addEventListener('change', onChange);
      return () => list.removeEventListener('change', onChange);
    },
    () => (typeof window !== 'undefined' && !!window.matchMedia ? window.matchMedia(query).matches : false),
    () => false,
  );
}

/** Phones: tables become cards below this width. */
export const PHONE_QUERY = '(max-width: 640px)';
