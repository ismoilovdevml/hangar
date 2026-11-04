import { useMemo } from 'react';
import { can, canAdminRepo, type Action, type Domain } from '../lib/privileges';
import { useSession } from './hooks';

export interface Permissions {
  can: (action: Action, domain: Domain, name: string) => boolean;
  canAdminRepo: (repo: string) => boolean;
}

/**
 * What the signed-in user may do, from the session's effective privileges.
 * A server that does not report privileges gets no client-side gating: the
 * buttons stay enabled and the server answers 403 when it must.
 */
export function usePermissions(): Permissions {
  const privileges = useSession().data?.privileges;
  return useMemo<Permissions>(() => {
    if (!privileges) return { can: () => true, canAdminRepo: () => true };
    return {
      can: (action, domain, name) => can(privileges, action, domain, name),
      canAdminRepo: (repo) => canAdminRepo(privileges, repo),
    };
  }, [privileges]);
}
