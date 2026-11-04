import { useEffect, useRef, useState } from 'react';
import type { CodeLine } from '../lib/clientSnippets';
import { copyText } from '../lib/clipboard';
import { IconCheck, IconCopy } from './icons';

interface CodeBlockProps {
  lines: CodeLine[];
  /** What the code is, used for the copy button label. */
  label: string;
}

export function CodeBlock({ lines, label }: CodeBlockProps) {
  const [copied, setCopied] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);

  const onCopy = async () => {
    const ok = await copyText(lines.map((l) => l.text).join('\n'));
    if (!ok) return;
    setCopied(true);
    clearTimeout(timer.current);
    timer.current = setTimeout(() => setCopied(false), 1800);
  };

  return (
    <div className="code-block">
      <pre>
        <code>
          {lines.map((l, i) => (
            <span key={i} className={l.comment ? 'code-block__comment' : undefined}>
              {l.text}
              {'\n'}
            </span>
          ))}
        </code>
      </pre>
      <button type="button" className="code-block__copy" onClick={onCopy} aria-label={`Copy ${label}`}>
        {copied ? <IconCheck size={16} /> : <IconCopy size={16} />}
      </button>
      <span className="visually-hidden" aria-live="polite">
        {copied ? 'Copied to clipboard' : ''}
      </span>
    </div>
  );
}
