import { cloneElement, type ReactElement, type ReactNode } from 'react';
import { initials } from '../../lib/format';
import { errorText } from '../../lib/apiError';
import { ErrorMessage } from '../../components/States';

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
  return <ErrorMessage error={error} message={errorText(error)} />;
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

/** A polite live region for "Token revoked." style confirmations. */
export function Notice({ message }: { message: string }) {
  return (
    <p className="notice" role="status" aria-live="polite">
      {message}
    </p>
  );
}

