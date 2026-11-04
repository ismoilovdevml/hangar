import { useEffect, useId, useRef, useState, type KeyboardEvent } from 'react';
import { IconCheck, IconChevronUpDown } from '../../components/icons';

export interface PickerOption<T extends string> {
  value: T;
  label: string;
}

interface PickerProps<T extends string> {
  /** Visible label inside the button, also the listbox's name. */
  label: string;
  value: T;
  options: readonly PickerOption<T>[];
  onChange: (value: T) => void;
}

/**
 * A single-choice filter: a button that opens a listbox (the WAI-ARIA
 * "select-only" pattern). Arrow keys, Home/End and typing a letter move,
 * Enter or Space choose, Escape and Tab close; focus returns to the button.
 */
export function Picker<T extends string>({ label, value, options, onChange }: PickerProps<T>) {
  const id = useId();
  const labelId = `${id}-label`;
  const buttonId = `${id}-button`;
  const listId = `${id}-list`;
  const optionId = (i: number) => `${id}-opt-${i}`;
  const [open, setOpen] = useState(false);
  const selected = Math.max(0, options.findIndex((o) => o.value === value));
  const [active, setActive] = useState(selected);
  const button = useRef<HTMLButtonElement>(null);
  const list = useRef<HTMLUListElement>(null);
  const root = useRef<HTMLDivElement>(null);
  const typed = useRef({ text: '', at: 0 });

  const show = (index = selected) => {
    setActive(index);
    setOpen(true);
  };
  const hide = (focusButton = true) => {
    setOpen(false);
    if (focusButton) button.current?.focus();
  };
  const choose = (i: number) => {
    const o = options[i];
    hide();
    if (o && o.value !== value) onChange(o.value);
  };

  useEffect(() => {
    if (open) list.current?.focus();
  }, [open]);

  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (root.current && !root.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener('mousedown', onDown);
    return () => document.removeEventListener('mousedown', onDown);
  }, [open]);

  useEffect(() => {
    if (open) document.getElementById(optionId(active))?.scrollIntoView?.({ block: 'nearest' });
    // optionId is derived from a stable id
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, active]);

  const onButtonKey = (e: KeyboardEvent<HTMLButtonElement>) => {
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      show();
    }
  };

  const onListKey = (e: KeyboardEvent<HTMLUListElement>) => {
    const last = options.length - 1;
    switch (e.key) {
      case 'ArrowDown':
        e.preventDefault();
        setActive((a) => Math.min(last, a + 1));
        return;
      case 'ArrowUp':
        e.preventDefault();
        setActive((a) => Math.max(0, a - 1));
        return;
      case 'Home':
        e.preventDefault();
        setActive(0);
        return;
      case 'End':
        e.preventDefault();
        setActive(last);
        return;
      case 'Enter':
      case ' ':
        e.preventDefault();
        choose(active);
        return;
      case 'Escape':
        e.preventDefault();
        e.stopPropagation();
        hide();
        return;
      case 'Tab':
        hide(false);
        return;
    }
    if (e.key.length === 1 && /\S/.test(e.key)) {
      const now = e.timeStamp;
      const t = typed.current;
      t.text = now - t.at > 700 ? e.key.toLowerCase() : t.text + e.key.toLowerCase();
      t.at = now;
      const from = t.text.length === 1 ? active + 1 : active;
      const order = [...options.keys()].map((k) => (k + from) % options.length);
      const hit = order.find((i) => options[i]!.label.toLowerCase().startsWith(t.text));
      if (hit !== undefined) setActive(hit);
    }
  };

  return (
    <div className="vpicker" ref={root}>
      <button
        ref={button}
        id={buttonId}
        type="button"
        className="filter-select vpicker__button"
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-controls={open ? listId : undefined}
        aria-labelledby={`${labelId} ${buttonId}`}
        onClick={() => (open ? hide() : show())}
        onKeyDown={onButtonKey}
      >
        <span id={labelId} className="filter-select__label">
          {label}
        </span>
        <span className="vpicker__value">{options[selected]?.label}</span>
        <IconChevronUpDown size={16} />
      </button>
      {open && (
        <ul
          ref={list}
          id={listId}
          role="listbox"
          tabIndex={-1}
          aria-labelledby={labelId}
          aria-activedescendant={optionId(active)}
          className="vpicker__list glass"
          onKeyDown={onListKey}
        >
          {options.map((o, i) => (
            <li
              key={o.value}
              id={optionId(i)}
              role="option"
              aria-selected={o.value === value}
              className={['vpicker__option', i === active ? 'is-active' : ''].filter(Boolean).join(' ')}
              onMouseEnter={() => setActive(i)}
              onMouseDown={(e) => e.preventDefault()}
              onClick={() => choose(i)}
            >
              <span>{o.label}</span>
              {o.value === value && <IconCheck size={16} />}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
