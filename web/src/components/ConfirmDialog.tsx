import { useEffect, useId, useRef, useState, type KeyboardEvent, type ReactNode } from 'react';
import { createPortal } from 'react-dom';

interface ConfirmDialogProps {
  title: string;
  children: ReactNode;
  confirmLabel: string;
  /** Destructive actions get a red confirm button; Cancel is focused first either way. */
  tone?: 'danger' | 'primary';
  busy?: boolean;
  error?: string | null;
  /** When set, the user must type this exact text (e.g. the name) before confirming. */
  typeToConfirm?: string;
  onConfirm: () => void;
  onCancel: () => void;
}

/**
 * Modal confirmation for irreversible or expensive actions. Render it only
 * while open; it traps focus, closes on Escape and returns focus on close.
 */
export function ConfirmDialog({
  title,
  children,
  confirmLabel,
  tone = 'primary',
  busy,
  error,
  typeToConfirm,
  onConfirm,
  onCancel,
}: ConfirmDialogProps) {
  const titleId = useId();
  const bodyId = useId();
  const inputId = useId();
  const dialogRef = useRef<HTMLDivElement>(null);
  const cancelRef = useRef<HTMLButtonElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const [typed, setTyped] = useState('');
  const ready = !typeToConfirm || typed === typeToConfirm;

  useEffect(() => {
    const previous = document.activeElement as HTMLElement | null;
    (inputRef.current ?? cancelRef.current)?.focus();
    return () => previous?.focus?.();
  }, []);

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (e.key === 'Escape' && !busy) {
      e.stopPropagation();
      onCancel();
      return;
    }
    if (e.key !== 'Tab') return;
    const focusables = dialogRef.current?.querySelectorAll<HTMLElement>('button:not([disabled]), [href], input, select, textarea');
    if (!focusables?.length) return;
    const first = focusables[0]!;
    const last = focusables[focusables.length - 1]!;
    if (e.shiftKey && document.activeElement === first) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault();
      first.focus();
    }
  };

  return createPortal(
    <div
      className="dialog-scrim"
      onMouseDown={(e) => {
        if (e.target === e.currentTarget && !busy) onCancel();
      }}
    >
      <div
        ref={dialogRef}
        className="dialog"
        role="alertdialog"
        aria-modal="true"
        aria-labelledby={titleId}
        aria-describedby={bodyId}
        onKeyDown={onKeyDown}
      >
        <h2 className="dialog__title" id={titleId}>
          {title}
        </h2>
        <div className="dialog__body" id={bodyId}>
          {children}
        </div>
        {typeToConfirm && (
          <div className="field">
            <label htmlFor={inputId} className="field__label">
              Type <code className="inline-code">{typeToConfirm}</code> to confirm
            </label>
            <input
              ref={inputRef}
              id={inputId}
              className="input input--mono"
              autoComplete="off"
              autoCapitalize="off"
              spellCheck={false}
              value={typed}
              onChange={(e) => setTyped(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter' && ready && !busy) {
                  e.preventDefault();
                  onConfirm();
                }
              }}
            />
          </div>
        )}
        {error && (
          <p className="form-error" role="alert">
            {error}
          </p>
        )}
        <div className="dialog__actions">
          <button ref={cancelRef} type="button" className="button button--secondary" onClick={onCancel} disabled={busy}>
            Cancel
          </button>
          <button
            type="button"
            className={tone === 'danger' ? 'button button--danger' : 'button button--primary'}
            onClick={onConfirm}
            disabled={busy || !ready}
            aria-busy={busy || undefined}
          >
            {busy ? 'Working…' : confirmLabel}
          </button>
        </div>
      </div>
    </div>,
    document.body,
  );
}
