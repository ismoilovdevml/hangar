import { useQuery } from '@tanstack/react-query';
import { Outlet } from '@tanstack/react-router';
import { sessionQuery } from '../../api/hooks';
import { AppShell } from '../../components/AppShell';

/**
 * Layout of the public documentation pages: inside the app shell (sidebar,
 * breadcrumb) for signed-in users, standalone over the wallpaper otherwise.
 */
export function DocsShell() {
  const session = useQuery(sessionQuery);
  // Wait for the first answer only: a refetch after a 401 must not unmount the page.
  if (session.dataUpdatedAt === 0 && session.errorUpdatedAt === 0) return null;
  // A user who must change their password gets no app shell yet.
  return session.data && !session.data.password_change_required ? <AppShell /> : <Outlet />;
}
