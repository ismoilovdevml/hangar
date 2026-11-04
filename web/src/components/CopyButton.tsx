import { useEffect, useRef, useState } from 'react';
import { copyText } from '../lib/clipboard';
import { IconCheck, IconCopy } from './icons';

/** Icon button that copies `text`; the result is announced to screen readers. */
export function CopyButton({ text, label, className }: { text: string; label: string; className?: string }) {
  const [copied, setCopied] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);

  const onCopy = async () => {
    if (!(await copyText(text))) return;
    setCopied(true);
    clearTimeout(timer.current);
    timer.current = setTimeout(() => setCopied(false), 1800);
  };

  return (
    <>
      <button type="button" className={['copy-button', className ?? ''].join(' ')} onClick={onCopy} aria-label={label}>
        {copied ? <IconCheck size={16} /> : <IconCopy size={16} />}
      </button>
      <span className="visually-hidden" aria-live="polite">
        {copied ? 'Copied to clipboard' : ''}
      </span>
    </>
  );
}
