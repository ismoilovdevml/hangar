import { useEffect, useId, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from 'react';
import { createPortal } from 'react-dom';
import { insideModal, useAnchoredLayer } from './anchoredLayer';
import { IconCheck, IconChevronUpDown, IconSearch } from './icons';

export interface SelectOption<T extends string = string> {
  value: T;
  label: string;
  /** Secondary text under the label, also matched by the search box. */
  hint?: string;
  disabled?: boolean;
  /** Decorative mark before the label (a format logo), also shown in the trigger. */
  icon?: ReactNode;
}

interface SelectProps<T extends string> {
  value: T;
  options: readonly SelectOption<T>[];
  onChange: (value: T) => void;
  /** Id of the trigger, for `<label htmlFor>`. */
  id?: string;
  /** Shown when the value matches no option. */
  placeholder?: string;
  disabled?: boolean;
  /** Submits the value with the enclosing form under this name. */
  name?: string;
  /** A filter box above the options; on by default for more than 10 options. */
  searchable?: boolean;
  /** `field`: a full form control. `inline`: borderless, inside a `.filter-select` pill. */
  variant?: 'field' | 'inline';
  size?: 'md' | 'sm';
  mono?: boolean;
  className?: string;
  /** Rendered before the label in the trigger (an icon, a format badge). */
  prefix?: ReactNode;
  /** Fires when focus leaves the closed control (to mark a field as touched). */
  onBlur?: () => void;
  'aria-label'?: string;
  'aria-labelledby'?: string;
  'aria-describedby'?: string;
  'aria-invalid'?: boolean | 'true' | 'false';
}

const TYPEAHEAD_RESET_MS = 600;

/**
 * A single-choice picker styled from the design tokens: a select-only combobox
 * (WAI-ARIA) with a listbox popup in the top layer, so no panel, table or
 * dialog clips it.
 *
 * Keyboard: Enter, Space, Alt+Down or the arrows open it; arrows, Home, End,
 * PageUp and PageDown move; Enter or Space picks; Escape and Tab close; typing
 * jumps to the first option starting with the typed text.
 */
export function Select<T extends string>({
  value,
  options,
  onChange,
  id,
  placeholder = 'Select…',
  disabled,
  name,
  searchable,
  variant = 'field',
  size = 'md',
  mono,
  className,
  prefix,
  onBlur,
  'aria-label': ariaLabel,
  'aria-labelledby': ariaLabelledBy,
  'aria-describedby': ariaDescribedBy,
  'aria-invalid': ariaInvalid,
}: SelectProps<T>) {
  const autoId = useId();
  const triggerId = id ?? `${autoId}-trigger`;
  const listId = `${autoId}-list`;
  const optionId = (i: number) => `${autoId}-opt-${i}`;
  const triggerRef = useRef<HTMLButtonElement>(null);
  const layerRef = useRef<HTMLDivElement>(null);
  const searchRef = useRef<HTMLInputElement>(null);
  const optionRefs = useRef<(HTMLLIElement | null)[]>([]);
  const typeahead = useRef({ text: '', at: 0 });

  const [open, setOpen] = useState(false);
  const [inModal, setInModal] = useState(false);
  const [query, setQuery] = useState('');
  const [active, setActive] = useState(-1);
  const withSearch = searchable ?? options.length > 10;

  const visible = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q) return options;
    return options.filter((o) => o.label.toLowerCase().includes(q) || o.hint?.toLowerCase().includes(q) || o.value.toLowerCase().includes(q));
  }, [options, query]);

  const selected = options.find((o) => o.value === value);
  useAnchoredLayer(triggerRef, layerRef, open);

  // Close on a press outside the trigger and the popup.
  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      const t = e.target as Node;
      if (triggerRef.current?.contains(t) || layerRef.current?.contains(t)) return;
      setOpen(false);
    };
    document.addEventListener('mousedown', onDown);
    return () => document.removeEventListener('mousedown', onDown);
  }, [open]);

  useEffect(() => {
    if (open && active >= 0) optionRefs.current[active]?.scrollIntoView({ block: 'nearest' });
  }, [open, active]);

  useEffect(() => {
    if (open && withSearch) searchRef.current?.focus();
  }, [open, withSearch]);

  const enabledFrom = (list: readonly SelectOption<T>[], start: number, step: 1 | -1) => {
    for (let i = start; i >= 0 && i < list.length; i += step) if (!list[i]!.disabled) return i;
    return -1;
  };

  const openList = (at?: 'first' | 'last' | number) => {
    if (disabled) return;
    setInModal(insideModal(triggerRef.current));
    setQuery('');
    const current = options.findIndex((o) => o.value === value);
    let start = typeof at === 'number' ? at : current;
    if (at === 'first' || start < 0) start = enabledFrom(options, 0, 1);
    if (at === 'last') start = enabledFrom(options, options.length - 1, -1);
    setActive(start);
    setOpen(true);
  };

  const close = (focusTrigger = true) => {
    setOpen(false);
    setQuery('');
    if (focusTrigger) triggerRef.current?.focus();
  };

  const pick = (o: SelectOption<T> | undefined) => {
    if (!o || o.disabled) return;
    if (o.value !== value) onChange(o.value);
    close();
  };

  const move = (to: number, step: 1 | -1) => {
    const clamped = Math.max(0, Math.min(visible.length - 1, to));
    const next = enabledFrom(visible, clamped, step);
    if (next >= 0) setActive(next);
    else {
      const back = enabledFrom(visible, clamped, step === 1 ? -1 : 1);
      if (back >= 0) setActive(back);
    }
  };

  /** Index of the next option whose label starts with what has been typed. */
  const matchTyped = (char: string, from: number) => {
    const now = Date.now();
    const t = typeahead.current;
    t.text = now - t.at > TYPEAHEAD_RESET_MS ? char : t.text + char;
    t.at = now;
    const text = t.text.toLowerCase();
    // Repeating one letter cycles through the options starting with it.
    const cycling = text.length > 1 && [...text].every((c) => c === text[0]);
    const needle = cycling ? text[0]! : text;
    const n = options.length;
    const begin = cycling || text.length === 1 ? from + 1 : Math.max(from, 0);
    for (let k = 0; k < n; k++) {
      const i = (begin + k + n) % n;
      const o = options[i]!;
      if (!o.disabled && o.label.toLowerCase().startsWith(needle)) return i;
    }
    return -1;
  };

  const onListKey = (e: KeyboardEvent<HTMLElement>, fromSearch: boolean) => {
    switch (e.key) {
      case 'ArrowDown':
        e.preventDefault();
        move(active < 0 ? 0 : active + 1, 1);
        return;
      case 'ArrowUp':
        e.preventDefault();
        if (e.altKey) pick(visible[active]);
        else move(active < 0 ? visible.length - 1 : active - 1, -1);
        return;
      case 'PageDown':
        e.preventDefault();
        move(active + 10, 1);
        return;
      case 'PageUp':
        e.preventDefault();
        move(active - 10, -1);
        return;
      case 'Home':
        if (fromSearch) return;
        e.preventDefault();
        move(0, 1);
        return;
      case 'End':
        if (fromSearch) return;
        e.preventDefault();
        move(visible.length - 1, -1);
        return;
      case 'Enter':
        e.preventDefault();
        pick(visible[active]);
        return;
      case 'Escape':
        e.preventDefault();
        e.stopPropagation();
        close();
        return;
      case 'Tab':
        // Focus goes back to the trigger first, so Tab continues from there.
        close();
        return;
    }
    if (fromSearch) return;
    if (e.key === ' ' && Date.now() - typeahead.current.at > TYPEAHEAD_RESET_MS) {
      e.preventDefault();
      pick(visible[active]);
      return;
    }
    if (e.key.length === 1 && !e.ctrlKey && !e.metaKey && !e.altKey) {
      e.preventDefault();
      const i = matchTyped(e.key, active);
      if (i >= 0) setActive(i);
    }
  };

  const onTriggerKey = (e: KeyboardEvent<HTMLButtonElement>) => {
    if (open) {
      onListKey(e, false);
      return;
    }
    switch (e.key) {
      case 'ArrowDown':
      case 'ArrowUp':
      case 'Enter':
      case ' ':
        e.preventDefault();
        openList();
        return;
      case 'Home':
        e.preventDefault();
        openList('first');
        return;
      case 'End':
        e.preventDefault();
        openList('last');
        return;
    }
    if (e.key.length === 1 && !e.ctrlKey && !e.metaKey && !e.altKey) {
      e.preventDefault();
      const i = matchTyped(e.key, options.findIndex((o) => o.value === value));
      openList(i >= 0 ? i : undefined);
    }
  };

  const classes = ['select', `select--${variant}`, size === 'sm' ? 'select--sm' : '', mono ? 'select--mono' : '', className ?? '']
    .filter(Boolean)
    .join(' ');
  const activeId = open && active >= 0 && visible[active] ? optionId(active) : undefined;

  return (
    <>
      <button
        ref={triggerRef}
        id={triggerId}
        type="button"
        role="combobox"
        className={classes}
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-controls={listId}
        aria-activedescendant={withSearch ? undefined : activeId}
        aria-label={ariaLabel}
        aria-labelledby={ariaLabelledBy}
        aria-describedby={ariaDescribedBy}
        aria-invalid={ariaInvalid}
        data-value={value}
        disabled={disabled}
        onClick={() => (open ? close() : openList())}
        onBlur={() => {
          if (!open) onBlur?.();
        }}
        onKeyDown={onTriggerKey}
        onKeyUp={(e) => {
          // Space activates buttons on key up; the key down already opened or picked.
          if (e.key === ' ') e.preventDefault();
        }}
      >
        {prefix}
        {selected?.icon && <span className="select__icon">{selected.icon}</span>}
        <span className={selected ? 'select__value' : 'select__value select__placeholder'}>{selected?.label ?? placeholder}</span>
        <IconChevronUpDown size={14} />
      </button>
      {name && <input type="hidden" name={name} value={value} />}
      {open &&
        createPortal(
          <div
            ref={layerRef}
            className={inModal ? 'select-popup select-popup--modal' : 'select-popup'}
            onMouseDown={(e) => {
              // Keep focus on the trigger (or the search box) while choosing.
              if (e.target !== searchRef.current) e.preventDefault();
            }}
          >
            {withSearch && (
              <div className="select-popup__search">
                <IconSearch size={15} />
                <input
                  ref={searchRef}
                  type="search"
                  className="select-popup__search-input"
                  placeholder="Filter…"
                  aria-label="Filter options"
                  aria-controls={listId}
                  aria-activedescendant={activeId}
                  autoComplete="off"
                  spellCheck={false}
                  value={query}
                  onChange={(e) => {
                    setQuery(e.target.value);
                    setActive(0);
                  }}
                  onKeyDown={(e) => onListKey(e, true)}
                />
              </div>
            )}
            <ul
              id={listId}
              role="listbox"
              className="select-popup__list"
              aria-label={ariaLabel}
              aria-labelledby={ariaLabel ? undefined : (ariaLabelledBy ?? triggerId)}
              tabIndex={-1}
            >
              {visible.map((o, i) => {
                const isSelected = o.value === value;
                return (
                  <li
                    key={o.value}
                    id={optionId(i)}
                    ref={(el) => {
                      optionRefs.current[i] = el;
                    }}
                    role="option"
                    aria-selected={isSelected}
                    aria-disabled={o.disabled || undefined}
                    data-value={o.value}
                    className={['select-option', i === active ? 'is-active' : '', isSelected ? 'is-selected' : ''].filter(Boolean).join(' ')}
                    onMouseMove={() => {
                      if (i !== active && !o.disabled) setActive(i);
                    }}
                    onClick={() => pick(o)}
                  >
                    {o.icon && <span className="select-option__icon">{o.icon}</span>}
                    <span className="select-option__text">
                      <span className="select-option__label">{o.label}</span>
                      {o.hint && <span className="select-option__hint">{o.hint}</span>}
                    </span>
                    {isSelected && <IconCheck size={16} />}
                  </li>
                );
              })}
            </ul>
            {visible.length === 0 && (
              <p className="select-popup__empty" role="status">
                No matches for “{query.trim()}”
              </p>
            )}
          </div>,
          document.body,
        )}
    </>
  );
}
