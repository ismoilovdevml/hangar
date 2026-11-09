import { useEffect, type ReactNode } from 'react';
import { IconCheck, IconClose } from './icons';

/**
 * A short confirmation at the bottom of the screen, announced politely. It
 * stays until closed or `timeoutMs` passes; actions inside it stay reachable.
 */
export function Toast({ children, onClose, timeoutMs = 10_000 }: { children: ReactNode; onClose: () => void; timeoutMs?: number }) {
  useEffect(() => {
    const t = setTimeout(onClose, timeoutMs);
    return () => clearTimeout(t);
  }, [onClose, timeoutMs]);
  return (
    <div className="toast glass" role="status">
      <span className="toast__icon" aria-hidden="true">
        <IconCheck size={16} />
      </span>
      <div className="toast__body">{children}</div>
      <button type="button" className="icon-button icon-button--sm toast__close" aria-label="Dismiss" onClick={onClose}>
        <IconClose size={16} />
      </button>
    </div>
  );
}
