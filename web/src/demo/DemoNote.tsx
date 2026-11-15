import { useRouterState } from '@tanstack/react-router';

/**
 * Styles of the note. They travel with this demo-only chunk instead of a
 * style sheet, so the product's style sheets stay as they are.
 */
const STYLE = `
.demo-note {
  position: fixed;
  right: 12px;
  bottom: 12px;
  z-index: 60;
  display: flex;
  gap: 8px;
  align-items: center;
  margin: 0;
  padding: 5px 11px;
  border: 1px solid var(--hairline-strong);
  border-radius: 999px;
  background: var(--glass-bg-strong);
  box-shadow: var(--shadow-raised);
  color: var(--ink-3);
  font-size: 12px;
  line-height: 1.4;
  pointer-events: none;
  backdrop-filter: var(--glass-blur);
}
.demo-note b {
  color: var(--lake-ink);
  font-weight: 700;
}
`;

/** The small note the public demo shows on every page. */
export default function DemoNote() {
  const onLogin = useRouterState({ select: (s) => s.location.pathname === '/login' });
  return (
    <>
      <style>{STYLE}</style>
      <p className="demo-note" role="note">
        <b>Demo</b>
        {onLogin ? 'Sign in with demo / demo' : 'Sample data, changes reset on reload'}
      </p>
    </>
  );
}
