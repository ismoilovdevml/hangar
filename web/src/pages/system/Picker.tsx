import { useEffect, useId, useRef, useState, type KeyboardEvent, type ReactNode } from 'react';
import { IconChevronUpDown } from '../../components/icons';

export interface PickerOption<T extends string> {
  value: T;
  label: string;
  /** Shown after the label in the list (e.g. "this node"). */
  hint?: ReactNode;
}

interface PickerProps<T extends string> {
  label: string;
  options: PickerOption<T>[];
  value: T;
  onChange: (value: T) => void;
  disabled?: boolean;
}

/**
 * Select-only combobox (WAI-ARIA APG pattern): a button showing the choice
 * and a listbox below it. Focus stays on the button; the active option is
 * announced through aria-activedescendant. Keys: Arrow keys, Home, End,
 * Enter/Space to choose, Escape to close, letters jump to an option.
 */
export function Picker<T extends string>({ label, options, value, onChange, disabled }: PickerProps<T>) {
  const id = useId();
  const labelId = `${id}-label`;
  const listId = `${id}-list`;
  const [open, setOpen] = useState(false);
  const selected = Math.max(
    0,
    options.findIndex((o) => o.value === value),
  );
  const [active, setActive] = useState(selected);
  const root = useRef<HTMLDivElement>(null);
  const typed = useRef({ text: '', at: 0 });

  useEffect(() => {
    if (!open) return;
    const close = (e: MouseEvent) => {
      if (!root.current?.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener('mousedown', close);
    return () => document.removeEventListener('mousedown', close);
  }, [open]);

  useEffect(() => {
    if (open) document.getElementById(`${id}-opt-${active}`)?.scrollIntoView({ block: 'nearest' });
  }, [open, active, id]);

  const show = (index = selected) => {
    setActive(index);
    setOpen(true);
  };
  const choose = (index: number) => {
    const o = options[index];
    setOpen(false);
    if (o && o.value !== value) onChange(o.value);
  };

  const onKeyDown = (e: KeyboardEvent<HTMLButtonElement>) => {
    const last = options.length - 1;
    switch (e.key) {
      case 'ArrowDown':
        e.preventDefault();
        if (!open) show();
        else setActive((a) => Math.min(last, a + 1));
        return;
      case 'ArrowUp':
        e.preventDefault();
        if (!open) show();
        else setActive((a) => Math.max(0, a - 1));
        return;
      case 'Home':
        e.preventDefault();
        show(0);
        return;
      case 'End':
        e.preventDefault();
        show(last);
        return;
      case 'Enter':
      case ' ':
        e.preventDefault();
        if (open) choose(active);
        else show();
        return;
      case 'Escape':
        if (open) {
          e.preventDefault();
          setOpen(false);
        }
        return;
      case 'Tab':
        if (open) choose(active);
        return;
    }
    if (e.key.length === 1 && /\S/.test(e.key)) {
      const now = Date.now();
      typed.current = { text: (now - typed.current.at < 600 ? typed.current.text : '') + e.key.toLowerCase(), at: now };
      const start = open ? active : selected;
      const order = options.map((_, i) => (start + 1 + i) % options.length);
      const hit = order.find((i) => options[i]!.label.toLowerCase().startsWith(typed.current.text));
      if (hit !== undefined) show(hit);
    }
  };

  const current = options[selected];
  return (
    <div className={open ? 'picker is-open' : 'picker'} ref={root}>
      <span id={labelId} className="picker__label">
        {label}
      </span>
      <button
        type="button"
        role="combobox"
        className="picker__button"
        aria-labelledby={`${labelId} ${id}-value`}
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-controls={listId}
        aria-activedescendant={open ? `${id}-opt-${active}` : undefined}
        disabled={disabled}
        onClick={() => (open ? setOpen(false) : show())}
        onKeyDown={onKeyDown}
      >
        <span id={`${id}-value`} className="picker__value">
          {current?.label ?? ''}
        </span>
        <IconChevronUpDown size={16} />
      </button>
      {open && (
        <ul id={listId} role="listbox" aria-labelledby={labelId} className="picker__list glass" tabIndex={-1}>
          {options.map((o, i) => (
            <li
              key={o.value}
              id={`${id}-opt-${i}`}
              role="option"
              aria-selected={i === selected}
              className={['picker__option', i === active ? 'is-active' : ''].join(' ')}
              onMouseEnter={() => setActive(i)}
              onMouseDown={(e) => e.preventDefault()}
              onClick={() => choose(i)}
            >
              <span>{o.label}</span>
              {o.hint && <span className="picker__hint">{o.hint}</span>}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
