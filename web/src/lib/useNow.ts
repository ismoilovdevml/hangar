import { useEffect, useState } from 'react';

/**
 * Current time, refreshed on an interval so relative times stay accurate.
 * `null` stops the clock (nothing on screen counts); with `until`, it stops
 * once that moment (ms since the epoch) has passed.
 */
export function useNow(intervalMs: number | null = 30_000, until?: number): Date {
  const [now, setNow] = useState(() => new Date());
  useEffect(() => {
    if (intervalMs === null) return;
    const id = setInterval(() => {
      const d = new Date();
      setNow(d);
      if (until !== undefined && d.getTime() >= until) clearInterval(id);
    }, intervalMs);
    return () => clearInterval(id);
  }, [intervalMs, until]);
  return now;
}
