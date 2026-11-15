import { useEffect, useRef } from 'react';

/**
 * Escape closes only the topmost open layer (dialog, drawer, popover). Each
 * layer registers while it is open; one document listener hands Escape to
 * the layer registered last, so a dialog opened from a drawer closes alone.
 *
 * Controls that handle Escape themselves (a listbox, a search field) stop the
 * event before it reaches the document, so they still win.
 */
const stack: { current: () => void }[] = [];

function onKeyDown(e: KeyboardEvent) {
  if (e.key !== 'Escape' || e.defaultPrevented) return;
  const top = stack[stack.length - 1];
  if (!top) return;
  e.preventDefault();
  e.stopImmediatePropagation();
  top.current();
}

/** Registers an open layer; `onEscape` runs when Escape reaches it first. */
export function useEscapeLayer(onEscape: () => void, active = true) {
  const handler = useRef(onEscape);
  useEffect(() => {
    handler.current = onEscape;
  }, [onEscape]);

  useEffect(() => {
    if (!active) return;
    const entry = { current: () => handler.current() };
    if (stack.length === 0) document.addEventListener('keydown', onKeyDown);
    stack.push(entry);
    return () => {
      const i = stack.indexOf(entry);
      if (i >= 0) stack.splice(i, 1);
      if (stack.length === 0) document.removeEventListener('keydown', onKeyDown);
    };
  }, [active]);
}
