import { useEffect, useRef, useState } from 'react';
import { copyText } from '../../lib/clipboard';

interface CopyTextButtonProps {
  text: string;
  label: string;
  copiedLabel?: string;
  className?: string;
  /** Accessible name when the visible label is not specific enough. */
  ariaLabel?: string;
  disabled?: boolean;
}

/** A text button that copies `text` and says so for a moment; announced to screen readers. */
export function CopyTextButton({ text, label, copiedLabel = 'Copied', className, ariaLabel, disabled }: CopyTextButtonProps) {
  const [state, setState] = useState<'idle' | 'copied' | 'failed'>('idle');
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);

  const onCopy = async () => {
    const ok = await copyText(text);
    setState(ok ? 'copied' : 'failed');
    clearTimeout(timer.current);
    timer.current = setTimeout(() => setState('idle'), 1800);
  };

  return (
    <>
      <button type="button" className={className} onClick={() => void onCopy()} aria-label={ariaLabel} disabled={disabled}>
        {state === 'copied' ? copiedLabel : state === 'failed' ? 'Copy failed' : label}
      </button>
      <span className="visually-hidden" aria-live="polite">
        {state === 'copied' ? 'Copied to clipboard' : state === 'failed' ? 'Could not copy' : ''}
      </span>
    </>
  );
}
