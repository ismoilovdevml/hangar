import { useEffect, type RefObject } from 'react';
import { useEscapeLayer } from './layers';

const FOCUSABLE = 'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

interface ModalOptions {
  /** False while the question must be answered: Escape is taken, but does not close. */
  dismissible?: boolean;
  /** Where focus goes on open: `[data-autofocus]` or the first control, or the modal itself (a drawer to read). */
  focus?: 'first' | 'container';
}

/**
 * What every modal layer does while it is open: Escape closes only the
 * topmost layer, focus moves in and back to the opener on close, Tab stays
 * inside, and the page under it does not scroll. `ref` is the modal element
 * (it needs tabIndex={-1}).
 */
export function useModal(ref: RefObject<HTMLElement | null>, onClose: () => void, { dismissible = true, focus = 'first' }: ModalOptions = {}) {
  // A modal that cannot be dismissed still takes Escape, so nothing underneath closes instead.
  useEscapeLayer(dismissible ? onClose : () => {});

  useEffect(() => {
    const opener = document.activeElement as HTMLElement | null;
    const el = ref.current;
    const first = focus === 'first' ? (el?.querySelector<HTMLElement>('[data-autofocus]') ?? el?.querySelector<HTMLElement>(FOCUSABLE)) : null;
    (first ?? el)?.focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Tab' || !el) return;
      const items = [...el.querySelectorAll<HTMLElement>(FOCUSABLE)].filter((n) => n.offsetParent !== null || n === document.activeElement);
      if (items.length === 0) return;
      const firstItem = items[0]!;
      const lastItem = items[items.length - 1]!;
      if (e.shiftKey && (document.activeElement === firstItem || document.activeElement === el)) {
        e.preventDefault();
        lastItem.focus();
      } else if (!e.shiftKey && document.activeElement === lastItem) {
        e.preventDefault();
        firstItem.focus();
      }
    };
    document.addEventListener('keydown', onKey);
    const prevOverflow = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    return () => {
      document.removeEventListener('keydown', onKey);
      document.body.style.overflow = prevOverflow;
      if (opener && document.contains(opener)) opener.focus();
    };
    // The ref object and the focus rule are fixed for the life of a modal: this runs once per open.
  }, [ref, focus]);
}
