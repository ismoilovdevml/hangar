import { forwardRef, useEffect, useId, useImperativeHandle, useRef, useState, type FormEvent, type KeyboardEvent } from 'react';
import { IconSearch } from '../../components/icons';
import { applySuggestion, suggest, type SuggestContext, type Suggestion } from '../../lib/searchQuery';

interface QueryBarProps {
  value: string;
  onSubmit: (q: string) => void;
  context: SuggestContext;
  /** ArrowDown with no suggestion open moves focus into the results. */
  onArrowDown?: () => void;
}

export interface QueryBarHandle {
  focus: () => void;
}

/** Search box with qualifier completion (combobox with a listbox of suggestions). */
export const QueryBar = forwardRef<QueryBarHandle, QueryBarProps>(function QueryBar({ value, onSubmit, context, onArrowDown }, ref) {
  const [draft, setDraft] = useState(value);
  const [synced, setSynced] = useState(value);
  const [caret, setCaret] = useState(value.length);
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(0);
  const input = useRef<HTMLInputElement>(null);
  const listId = useId();
  const optionId = (i: number) => `${listId}-opt-${i}`;

  useImperativeHandle(ref, () => ({ focus: () => input.current?.focus() }), []);

  // The URL changed elsewhere (facet click, back button): show it.
  if (value !== synced) {
    setSynced(value);
    setDraft(value);
  }

  const suggestions: Suggestion[] = open ? suggest(draft, caret, context) : [];
  const shown = suggestions.length > 0;
  const safeActive = Math.min(active, Math.max(0, suggestions.length - 1));

  const pendingCaret = useRef<number | null>(null);
  useEffect(() => {
    if (pendingCaret.current === null || !input.current) return;
    input.current.setSelectionRange(pendingCaret.current, pendingCaret.current);
    pendingCaret.current = null;
  });

  const accept = (s: Suggestion) => {
    const next = applySuggestion(draft, caret, s);
    setDraft(next.q);
    setCaret(next.caret);
    setActive(0);
    pendingCaret.current = next.caret;
    setOpen(s.partial);
    input.current?.focus();
  };

  const submit = (e: FormEvent) => {
    e.preventDefault();
    setOpen(false);
    onSubmit(draft);
  };

  const onKeyDown = (e: KeyboardEvent<HTMLInputElement>) => {
    if (shown) {
      if (e.key === 'ArrowDown') {
        e.preventDefault();
        setActive((safeActive + 1) % suggestions.length);
        return;
      }
      if (e.key === 'ArrowUp') {
        e.preventDefault();
        setActive((safeActive - 1 + suggestions.length) % suggestions.length);
        return;
      }
      if (e.key === 'Enter' || e.key === 'Tab') {
        const s = suggestions[safeActive];
        if (s) {
          e.preventDefault();
          accept(s);
        }
        return;
      }
      if (e.key === 'Escape') {
        e.preventDefault();
        setOpen(false);
        return;
      }
    } else if (e.key === 'ArrowDown' && onArrowDown) {
      e.preventDefault();
      onArrowDown();
    }
  };

  return (
    <form className="query-bar" role="search" onSubmit={submit}>
      <div className="query-bar__box">
        <IconSearch size={20} className="query-bar__icon" />
        <label htmlFor={`${listId}-input`} className="visually-hidden">
          Search query
        </label>
        <input
          ref={input}
          id={`${listId}-input`}
          className="query-bar__input"
          type="search"
          role="combobox"
          aria-autocomplete="list"
          aria-expanded={shown}
          aria-controls={listId}
          aria-activedescendant={shown ? optionId(safeActive) : undefined}
          aria-keyshortcuts="/"
          autoComplete="off"
          autoCapitalize="off"
          spellCheck={false}
          placeholder="Name, digest or qualifier, e.g. format:npm repo:npm-hosted"
          value={draft}
          onChange={(e) => {
            setDraft(e.target.value);
            setCaret(e.target.selectionStart ?? e.target.value.length);
            setActive(0);
            setOpen(true);
          }}
          onSelect={(e) => setCaret(e.currentTarget.selectionStart ?? draft.length)}
          onKeyDown={onKeyDown}
          onBlur={() => setOpen(false)}
        />
        <button type="submit" className="button button--primary query-bar__submit">
          Search
        </button>
      </div>
      <ul id={listId} role="listbox" aria-label="Suggestions" className="query-suggest" hidden={!shown}>
        {suggestions.map((s, i) => (
          <li
            key={s.insert}
            id={optionId(i)}
            role="option"
            aria-selected={i === safeActive}
            className={i === safeActive ? 'query-suggest__item is-active' : 'query-suggest__item'}
            // mousedown, so the input does not blur (and close the list) first
            onMouseDown={(e) => {
              e.preventDefault();
              accept(s);
            }}
            onMouseEnter={() => setActive(i)}
          >
            <code className="query-suggest__label">{s.label}</code>
            {s.hint && <span className="query-suggest__hint">{s.hint}</span>}
          </li>
        ))}
      </ul>
    </form>
  );
});
