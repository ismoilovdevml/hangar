import './demo.css';
import { useRouterState } from '@tanstack/react-router';

/** The small note the public demo shows on every page. */
export default function DemoNote() {
  const onLogin = useRouterState({ select: (s) => s.location.pathname === '/login' });
  return (
    <p className="demo-note" role="note">
      <b>Demo</b>
      {onLogin ? 'Sign in with demo / demo' : 'Sample data, changes reset on reload'}
    </p>
  );
}
