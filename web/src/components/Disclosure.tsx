import { useEffect, useId, useRef, useState, type ReactNode } from 'react';

interface DisclosureProps {
  /** Accessible name of the trigger button. */
  label: string;
  buttonClassName: string;
  buttonContent: ReactNode;
  /** Accessible name of the panel region. */
  panelLabel: string;
  panelClassName?: string;
  children: ReactNode | ((close: () => void) => ReactNode);
}

/** A button that toggles a small floating panel; closes on outside click and Escape. */
export function Disclosure({ label, buttonClassName, buttonContent, panelLabel, panelClassName, children }: DisclosureProps) {
  const [open, setOpen] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);
  const panelId = useId();

  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) setOpen(false);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setOpen(false);
    };
    document.addEventListener('mousedown', onDown);
    document.addEventListener('keydown', onKey);
    return () => {
      document.removeEventListener('mousedown', onDown);
      document.removeEventListener('keydown', onKey);
    };
  }, [open]);

  const close = () => setOpen(false);

  return (
    <div className="popover-anchor" ref={rootRef}>
      <button
        type="button"
        className={buttonClassName}
        aria-label={label}
        aria-expanded={open}
        aria-controls={panelId}
        onClick={() => setOpen((v) => !v)}
      >
        {buttonContent}
      </button>
      {open && (
        <div className={['popover', 'glass', panelClassName ?? ''].join(' ')} id={panelId} role="region" aria-label={panelLabel}>
          {typeof children === 'function' ? children(close) : children}
        </div>
      )}
    </div>
  );
}
