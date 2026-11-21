import { useEffect, useId, useRef, useState } from 'react';
import { IconSearch } from './icons';

/** How long typing must pause before the filter is applied. */
const FILTER_DELAY_MS = 300;

/**
 * Text typed locally and committed after a pause: `draft` follows every
 * keystroke, `onCommit` gets the trimmed text (undefined when empty) once
 * typing stops. A new `value` from outside (back button, a tab that clears
 * the filter) replaces the draft.
 */
function useDebouncedDraft(value: string, onCommit: (v: string | undefined) => void, delay = FILTER_DELAY_MS) {
  const [draft, setDraft] = useState(value);
  const [synced, setSynced] = useState(value);
  if (value !== synced) {
    setSynced(value);
    setDraft(value);
  }
  const commit = useRef(onCommit);
  useEffect(() => {
    commit.current = onCommit;
  }, [onCommit]);
  useEffect(() => {
    if (draft.trim() === value) return;
    const t = setTimeout(() => commit.current(draft.trim() || undefined), delay);
    return () => clearTimeout(t);
  }, [draft, value, delay]);
  return [draft, setDraft] as const;
}

interface FilterSearchProps {
  /** The applied filter (from the URL). */
  value: string;
  onChange: (q: string | undefined) => void;
  /** The accessible name; also the placeholder unless one is given. */
  label: string;
  placeholder?: string;
  maxLength?: number;
  className?: string;
}

/** The search field of a list toolbar: a magnifier, a labelled input, applied after a short pause. */
export function FilterSearch({ value, onChange, label, placeholder, maxLength, className }: FilterSearchProps) {
  const [draft, setDraft] = useDebouncedDraft(value, onChange);
  const id = useId();
  return (
    <div className={['filter-search', className].filter(Boolean).join(' ')}>
      <label htmlFor={id} className="visually-hidden">
        {label}
      </label>
      <IconSearch size={16} />
      <input
        id={id}
        className="filter-search__input"
        type="search"
        placeholder={placeholder ?? label}
        value={draft}
        maxLength={maxLength}
        onChange={(e) => setDraft(e.target.value)}
        autoComplete="off"
        spellCheck={false}
      />
    </div>
  );
}
