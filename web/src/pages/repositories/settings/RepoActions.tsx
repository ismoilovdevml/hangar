/**
 * Header actions of a repository that act on the server right away:
 * "Invalidate cache" (proxies) and "Scan now" (formats the scanner covers).
 */
import { useState } from 'react';
import { Link } from '@tanstack/react-router';
import { errorText } from '../../../lib/apiError';
import { useInvalidateCache } from '../../../api/catalog';
import { useScanningStatus, useScanRepository } from '../../../api/findings';
import { usePermissions } from '../../../api/permissions';
import type { Repository } from '../../../api/types';
import { ConfirmDialog } from '../../../components/ConfirmDialog';
import { Toast } from '../../../components/Toast';
import { scannable } from '../../../lib/vulnerabilities';

const plural = (n: number, one: string, many = `${one}s`) => `${n} ${n === 1 ? one : many}`;

export function RepoActions({ repo }: { repo: Repository }) {
  const perms = usePermissions();
  const canAdmin = perms.canAdminRepo(repo.name);
  const canScan = perms.can('write', 'repo', repo.name) || perms.can('write', 'system', 'security');
  const status = useScanningStatus();
  const invalidate = useInvalidateCache(repo.name);
  const scan = useScanRepository(repo.name);
  const [confirm, setConfirm] = useState(false);
  const [toast, setToast] = useState<string | null>(null);

  const showInvalidate = canAdmin && repo.type === 'proxy';
  const showScan = canScan && scannable(repo.format, status.data);

  return (
    <>
      {showInvalidate && (
        <button
          type="button"
          className="button button--secondary"
          onClick={() => {
            invalidate.reset();
            setConfirm(true);
          }}
        >
          Invalidate cache
        </button>
      )}
      {showScan && (
        <button
          type="button"
          className="button button--secondary"
          disabled={scan.isPending}
          onClick={() =>
            scan.mutate(undefined, {
              onSuccess: (r) =>
                setToast(
                  r.components
                    ? `Scan queued for ${plural(r.components, 'package version')}.`
                    : 'Nothing to scan: the repository holds no package versions yet.',
                ),
            })
          }
        >
          {scan.isPending ? 'Queuing scan…' : 'Scan now'}
        </button>
      )}
      {scan.isError && (
        <span className="form-error" role="alert">
          {errorText(scan.error, { action: 'scan this repository' })}
        </span>
      )}

      {confirm && (
        <ConfirmDialog
          title={`Invalidate the cache of ${repo.name}?`}
          confirmLabel="Invalidate cache"
          busy={invalidate.isPending}
          error={invalidate.isError ? errorText(invalidate.error, { action: 'invalidate the cache' }) : null}
          onCancel={() => setConfirm(false)}
          onConfirm={() =>
            invalidate.mutate(undefined, {
              onSuccess: (r) => {
                setConfirm(false);
                setToast(
                  `Cache invalidated: ${plural(r.negative_entries, 'remembered “not found” answer')} and ${plural(r.rate_limited_hosts, 'rate-limit hold')} dropped. The next request asks the remote again.`,
                );
              },
            })
          }
        >
          <p>
            The repository forgets the remote&apos;s “not found” answers and rate-limit holds it remembers, so the next request asks the remote again.
            Cached files are kept.
          </p>
        </ConfirmDialog>
      )}

      {toast && (
        <Toast onClose={() => setToast(null)}>
          {toast}
          {scan.isSuccess && scan.data.components > 0 && (
            <>
              {' '}
              <Link to="/vulnerabilities" search={{ repo: repo.name, tab: 'all' }}>
                Open vulnerabilities
              </Link>
            </>
          )}
        </Toast>
      )}
    </>
  );
}
