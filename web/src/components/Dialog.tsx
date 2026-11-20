import { useEffect, useId, useRef, type ReactNode } from 'react';
import { createPortal } from 'react-dom';
import { useEscapeLayer } from '../lib/layers';
import { IconClose } from './icons';

const FOCUSABLE = 'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

export interface DialogProps {
  title: ReactNode;
  description?: ReactNode;
  onClose: () => void;
  children: ReactNode;
  /**
   * `dialog`: centred, a head with a close button and a scrolling body.
   * `drawer`: the same, as a full-height panel on the right.
   * `plain`: one pane for a short question; title, text and buttons, no close button.
   */
  variant?: 'dialog' | 'drawer' | 'plain';
  wide?: boolean;
  /** False while the question must be answered or an action runs: no close button, and Escape and the scrim do nothing. */
  dismissible?: boolean;
  /** `alertdialog` for a confirmation that interrupts the user. */
  role?: 'dialog' | 'alertdialog';
  /** Extra classes on the dialog and on the scrim behind it. */
  className?: string;
  scrimClassName?: string;
}

/**
 * The one modal of the app. Focus moves in on open (to `[data-autofocus]`,
 * else the first control) and back to the opener on close, Tab stays inside,
 * Escape and the scrim close it. Escape closes only the topmost layer, so a
 * dialog opened from a drawer closes alone. Only the body scrolls; a
 * `.dialog-form` that ends with its `.dialog-actions` keeps those buttons on
 * screen at the bottom edge (dialog.css).
 */
export function Dialog({
  title,
  description,
  onClose,
  children,
  variant = 'dialog',
  wide,
  dismissible = true,
  role = 'dialog',
  className,
  scrimClassName,
}: DialogProps) {
  const ref = useRef<HTMLDivElement>(null);
  const titleId = useId();
  const descId = useId();
  // A dialog that cannot be dismissed still takes Escape, so nothing underneath closes instead.
  useEscapeLayer(dismissible ? onClose : () => {});

  useEffect(() => {
    const opener = document.activeElement as HTMLElement | null;
    const el = ref.current;
    const first = el?.querySelector<HTMLElement>('[data-autofocus]') ?? el?.querySelector<HTMLElement>(FOCUSABLE);
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
  }, []);

  const plain = variant === 'plain';
  const heading = (
    <h2 id={titleId} className="dialog__title">
      {title}
    </h2>
  );
  const desc = description && (
    <p id={descId} className="dialog__desc">
      {description}
    </p>
  );

  return createPortal(
    <div
      className={['dialog-scrim', variant === 'drawer' ? 'dialog-scrim--drawer' : '', scrimClassName ?? ''].filter(Boolean).join(' ')}
      onMouseDown={(e) => {
        if (dismissible && e.target === e.currentTarget) onClose();
      }}
    >
      <div
        ref={ref}
        role={role}
        aria-modal="true"
        aria-labelledby={titleId}
        aria-describedby={description ? descId : undefined}
        tabIndex={-1}
        className={['dialog', variant !== 'dialog' ? `dialog--${variant}` : '', wide ? 'dialog--wide' : '', className ?? ''].filter(Boolean).join(' ')}
      >
        {plain ? (
          <>
            {heading}
            {desc}
          </>
        ) : (
          <div className="dialog__head">
            <div className="dialog__titles">
              {heading}
              {desc}
            </div>
            {dismissible && (
              <button type="button" className="icon-button icon-button--sm" aria-label="Close" onClick={onClose}>
                <IconClose size={18} />
              </button>
            )}
          </div>
        )}
        <div className="dialog__body">{children}</div>
      </div>
    </div>,
    document.body,
  );
}
