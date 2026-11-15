import { useId, useRef, useState, type InputHTMLAttributes, type KeyboardEvent } from 'react';
import { createPortal } from 'react-dom';
import { insideModal, useAnchoredLayer } from './anchoredLayer';
import { matchSuggestions } from '../lib/suggestions';

type InputProps = Omit<InputHTMLAttributes<HTMLInputElement>, 'value' | 'onChange' | 'list' | 'role'>;

interface ComboboxProps extends InputProps {
  value: string;
  onChange: (value: string) => void;
  /** Suggestions; any other text may still be typed. */
  options: readonly string[];
  /** Most suggestions listed at once (the list scrolls beyond). */
  limit?: number;
}

/**
 * A text input with an app-styled suggestion list, in place of the native
 * <datalist> (whose popup the browser draws in its own colours). Free text
 * stays allowed. Keyboard: Down/Up move through the suggestions (opening
 * them), Enter picks the highlighted one, Escape closes the list.
 */
export function Combobox({ value, onChange, options, limit, className, onKeyDown, onBlur, onFocus, ...input }: ComboboxProps) {
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(-1);
  const [inModal, setInModal] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);
  const layerRef = useRef<HTMLDivElement>(null);
  const listId = useId();
  const optionId = (i: number) => `${listId}-o${i}`;
  const items = matchSuggestions(options, value, limit);
  const shown = open && items.length > 0;
  useAnchoredLayer(inputRef, layerRef, shown, 280);
  const mono = !!className?.includes('input--mono');

  const show = () => {
    setInModal(insideModal(inputRef.current));
    setOpen(true);
  };
  const pick = (v: string) => {
    onChange(v);
    setOpen(false);
    setActive(-1);
  };

  const onKey = (e: KeyboardEvent<HTMLInputElement>) => {
    onKeyDown?.(e);
    if (e.defaultPrevented) return;
    switch (e.key) {
      case 'ArrowDown':
      case 'ArrowUp': {
        e.preventDefault();
        if (!shown) {
          show();
          setActive(e.key === 'ArrowDown' ? 0 : items.length - 1);
          return;
        }
        const step = e.key === 'ArrowDown' ? 1 : -1;
        setActive((a) => (a + step + items.length) % items.length);
        return;
      }
      case 'Enter':
        // With nothing highlighted, Enter submits the form as usual.
        if (shown && active >= 0 && items[active] !== undefined) {
          e.preventDefault();
          pick(items[active]!);
        }
        return;
      case 'Escape':
        if (shown) {
          // Closes the list only, not a dialog around it.
          e.preventDefault();
          e.stopPropagation();
          setOpen(false);
          setActive(-1);
        }
        return;
    }
  };

  return (
    <>
      <input
        {...input}
        ref={inputRef}
        className={className ?? 'input'}
        value={value}
        role="combobox"
        aria-autocomplete="list"
        aria-expanded={shown}
        aria-controls={shown ? listId : undefined}
        aria-activedescendant={shown && active >= 0 ? optionId(active) : undefined}
        autoComplete="off"
        onChange={(e) => {
          onChange(e.target.value);
          setActive(-1);
          show();
        }}
        onFocus={(e) => {
          onFocus?.(e);
          show();
        }}
        onBlur={(e) => {
          onBlur?.(e);
          setOpen(false);
          setActive(-1);
        }}
        onKeyDown={onKey}
      />
      {shown &&
        createPortal(
          <div
            ref={layerRef}
            className={inModal ? 'select-popup select-popup--modal' : 'select-popup'}
            // Choosing with the mouse keeps the focus in the input.
            onMouseDown={(e) => e.preventDefault()}
          >
            <ul id={listId} role="listbox" className="select-popup__list" aria-label="Suggestions">
              {items.map((o, i) => (
                <li
                  key={o}
                  id={optionId(i)}
                  role="option"
                  aria-selected={i === active}
                  className={['select-option', mono ? 'select-option--mono' : '', i === active ? 'is-active' : ''].filter(Boolean).join(' ')}
                  onMouseMove={() => i !== active && setActive(i)}
                  onClick={() => pick(o)}
                >
                  <span className="select-option__label">{o}</span>
                </li>
              ))}
            </ul>
          </div>,
          document.body,
        )}
    </>
  );
}
