import { useState, type SyntheticEvent } from 'react';

/**
 * Props for a `<details>` that opens by itself while `wanted` holds (e.g. a
 * section with values in it) but otherwise stays as the user left it. Without
 * `onToggle`, React would close a section the user opened as soon as `wanted`
 * turned false again, say while they cleared a field inside it.
 */
export function useDetailsOpen(wanted: boolean): { open: boolean; onToggle: (e: SyntheticEvent<HTMLDetailsElement>) => void } {
  const [open, setOpen] = useState(wanted);
  const [prev, setPrev] = useState(wanted);
  if (wanted !== prev) {
    setPrev(wanted);
    if (wanted) setOpen(true);
  }
  return { open, onToggle: (e) => setOpen(e.currentTarget.open) };
}
