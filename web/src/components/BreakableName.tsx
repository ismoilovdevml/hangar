import { Fragment } from 'react';

/**
 * A package name that wraps after its separators (System.Security.
 * Cryptography.Xml, @acme/ui-kit, com.acme:ledger-core) instead of in the
 * middle of a word. The break points are <wbr>, so copying still gives the
 * name as it is.
 */
export function BreakableName({ name }: { name: string }) {
  const parts: string[] = [];
  let start = 0;
  for (let i = 0; i < name.length; i++) {
    if ('./:'.includes(name[i]!) && i > start) {
      parts.push(name.slice(start, i + 1));
      start = i + 1;
    }
  }
  if (start < name.length || parts.length === 0) parts.push(name.slice(start));
  return (
    <>
      {parts.map((p, i) => (
        <Fragment key={i}>
          {i > 0 && <wbr />}
          {p}
        </Fragment>
      ))}
    </>
  );
}
