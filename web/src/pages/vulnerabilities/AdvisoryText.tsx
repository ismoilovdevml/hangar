import { useId, useState } from 'react';
import { Markdown } from '../../components/Markdown';
import { advisoryExcerpt } from '../../lib/vulnerabilities';

/**
 * An advisory's description (OSV `details`, Markdown): rendered with the
 * README renderer, no raw HTML and no images, its opening shown first.
 */
export function AdvisoryText({ source }: { source: string }) {
  const [open, setOpen] = useState(false);
  const id = useId();
  const excerpt = advisoryExcerpt(source);
  return (
    <div className="advisory">
      <div id={id} className="advisory__text">
        <Markdown source={open || excerpt === null ? source : excerpt} noImages topHeading={4} />
      </div>
      {excerpt !== null && (
        <button type="button" className="button button--secondary button--sm advisory__toggle" aria-expanded={open} aria-controls={id} onClick={() => setOpen(!open)}>
          {open ? 'Show less' : 'Show full advisory'}
        </button>
      )}
    </div>
  );
}
