import { cloneElement, useEffect, useId, useRef, useState, type FormEvent, type ReactElement, type ReactNode } from 'react';
import { createPortal } from 'react-dom';
import { IconClose } from '../../components/icons';
import { initials } from '../../lib/format';
import { errorText } from './model';

const FOCUSABLE = 'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

interface DialogProps {
  title: ReactNode;
  description?: ReactNode;
  onClose: () => void;
  children: ReactNode;
  /** A side drawer instead of a centred dialog. */
  drawer?: boolean;
  wide?: boolean;
  footer?: ReactNode;
}

/**
 * Modal dialog: focus moves in on open and back to the opener on close, Tab
 * stays inside, Escape and the scrim close it.
 */
export function Dialog({ title, description, onClose, children, drawer, wide, footer }: DialogProps) {
  const ref = useRef<HTMLDivElement>(null);
  const titleId = useId();
  const descId = useId();
  const close = useRef(onClose);
  useEffect(() => {
    close.current = onClose;
  }, [onClose]);

  useEffect(() => {
    const opener = document.activeElement as HTMLElement | null;
    const el = ref.current;
    const first = el?.querySelector<HTMLElement>('[data-autofocus]') ?? el?.querySelector<HTMLElement>(FOCUSABLE);
    (first ?? el)?.focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.stopPropagation();
        close.current();
        return;
      }
      if (e.key !== 'Tab' || !el) return;
      const items = [...el.querySelectorAll<HTMLElement>(FOCUSABLE)].filter((n) => n.offsetParent !== null || n === document.activeElement);
      if (items.length === 0) return;
      const firstItem = items[0]!;
      const lastItem = items[items.length - 1]!;
      if (e.shiftKey && document.activeElement === firstItem) {
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

  return createPortal(
    <div
      className={drawer ? 'dialog-scrim dialog-scrim--drawer' : 'dialog-scrim'}
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <div
        ref={ref}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        aria-describedby={description ? descId : undefined}
        tabIndex={-1}
        className={['dialog', drawer ? 'dialog--drawer' : '', wide ? 'dialog--wide' : ''].filter(Boolean).join(' ')}
      >
        <div className="dialog__head">
          <div className="dialog__titles">
            <h2 id={titleId} className="dialog__title">
              {title}
            </h2>
            {description && (
              <p id={descId} className="dialog__desc">
                {description}
              </p>
            )}
          </div>
          <button type="button" className="icon-button icon-button--sm" aria-label="Close" onClick={onClose}>
            <IconClose size={18} />
          </button>
        </div>
        <div className="dialog__body">{children}</div>
        {footer && <div className="dialog__foot">{footer}</div>}
      </div>
    </div>,
    document.body,
  );
}

interface FieldProps {
  id: string;
  label: ReactNode;
  /** Shown instead of the hint and marks the control invalid. */
  error?: string | null | false;
  hint?: ReactNode;
  wide?: boolean;
  /** A button next to the control (e.g. "Generate"). */
  addon?: ReactNode;
  children: ReactElement<Record<string, unknown>>;
}

/**
 * A labelled form control. The label holds only its text, so the control's
 * accessible name stays stable; the hint or error is linked with
 * aria-describedby.
 */
export function Field({ id, label, error, hint, wide, addon, children }: FieldProps) {
  const msgId = `${id}-msg`;
  const control = cloneElement(children, {
    id,
    'aria-invalid': error ? true : undefined,
    'aria-describedby': error || hint ? msgId : undefined,
  });
  return (
    <div className={wide ? 'field field--wide' : 'field'}>
      <label htmlFor={id} className="field__label">
        {label}
      </label>
      {addon ? (
        <span className="input-with-button">
          {control}
          {addon}
        </span>
      ) : (
        control
      )}
      {error ? (
        <span id={msgId} className="field__error">
          {error}
        </span>
      ) : hint ? (
        <span id={msgId} className="field__hint">
          {hint}
        </span>
      ) : null}
    </div>
  );
}

export function FormError({ error }: { error: unknown }) {
  if (!error) return null;
  return (
    <p className="form-error" role="alert">
      {errorText(error)}
    </p>
  );
}

const AVATAR_TONES = ['info', 'peach', 'sage', 'violet', 'warn'] as const;

export function Avatar({ name, size = 36 }: { name: string; size?: number }) {
  let h = 0;
  for (const c of name) h = (h * 31 + c.charCodeAt(0)) >>> 0;
  const tone = AVATAR_TONES[h % AVATAR_TONES.length];
  return (
    <span className={`avatar tone-${tone}`} style={{ width: size, height: size }} aria-hidden="true">
      {initials(name)}
    </span>
  );
}

interface ConfirmProps {
  title: string;
  children: ReactNode;
  confirmLabel: string;
  danger?: boolean;
  /** Typing this text enables the confirm button (irreversible actions). */
  typeToConfirm?: string;
  onConfirm: () => Promise<unknown>;
  onClose: () => void;
}

/** Confirmation for destructive actions; shows the API error and stays open on failure. */
export function ConfirmDialog({ title, children, confirmLabel, danger, typeToConfirm, onConfirm, onClose }: ConfirmProps) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const [typed, setTyped] = useState('');
  const inputId = useId();
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await onConfirm();
      onClose();
    } catch (err) {
      setError(err);
      setBusy(false);
    }
  };
  const blocked = typeToConfirm !== undefined && typed !== typeToConfirm;
  return (
    <Dialog title={title} onClose={onClose}>
      <form className="dialog-form" onSubmit={submit}>
        <div className="dialog__text">{children}</div>
        {typeToConfirm !== undefined && (
          <Field id={inputId} label={<>Type <strong>{typeToConfirm}</strong> to confirm</>}>
            <input className="input" value={typed} onChange={(e) => setTyped(e.target.value)} autoComplete="off" />
          </Field>
        )}
        <FormError error={error} />
        <div className="dialog-actions">
          <button type="button" className="button button--secondary" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className={danger ? 'button button--danger' : 'button button--primary'} disabled={busy || blocked}>
            {busy ? 'Working…' : confirmLabel}
          </button>
        </div>
      </form>
    </Dialog>
  );
}

/** A polite live region for "Token revoked." style confirmations. */
export function Notice({ message }: { message: string }) {
  return (
    <p className="notice" role="status" aria-live="polite">
      {message}
    </p>
  );
}

