/**
 * The OpenPGP key that signs an APT or RPM hosted or group repository:
 * details, the public key URLs, rotation and import (write-only).
 */
import { useState } from 'react';
import { ApiRequestError } from '../../../api/client';
import { useChangeSigningKey, useSigningKey } from '../../../api/catalog';
import { usePermissions } from '../../../api/permissions';
import type { Repository } from '../../../api/types';
import { ConfirmDialog } from '../../../components/ConfirmDialog';
import { CopyButton } from '../../../components/CopyButton';
import { ErrorNote } from '../../../components/ErrorNote';
import { Panel } from '../../../components/Panel';
import { Pill } from '../../../components/Pill';
import { shortDate } from '../../../lib/vulnerabilities';

const ALGORITHMS: Record<string, string> = { rsa4096: 'RSA 4096', ed25519: 'Ed25519' };

/** Upper-case hex in groups of four, as gpg prints it. */
const groupHex = (fp: string) => fp.replace(/(.{4})(?=.)/g, '$1 ');

const isStatus = (err: unknown, status: number) => err instanceof ApiRequestError && err.status === status;

function changeError(err: unknown): string {
  if (err instanceof ApiRequestError) {
    if (err.status === 403) return 'You do not have permission to change this key.';
    if (err.status === 501) return 'The server has no secret key (HANGAR_SECRET_KEY), so it cannot store signing keys.';
    return err.body?.message ?? 'The server could not change the key.';
  }
  return 'Cannot reach the server. Check your connection and try again.';
}

export function SigningKeyPanel({ repo }: { repo: Repository }) {
  const shared = repo.config.format?.signing_key === 'shared';
  const keyKind = shared ? 'shared' : 'repository';
  const perms = usePermissions();
  const key = useSigningKey(repo.name, keyKind);
  const change = useChangeSigningKey(repo.name, keyKind);
  const [dialog, setDialog] = useState<'rotate' | 'import' | null>(null);
  const [privateKey, setPrivateKey] = useState('');
  const [passphrase, setPassphrase] = useState('');
  const [missingKey, setMissingKey] = useState(false);
  const [done, setDone] = useState('');

  const canChange = shared ? perms.can('admin', 'system', 'repos') : perms.canAdminRepo(repo.name);
  const reason = shared ? 'Changing the shared key needs system:repos:admin.' : 'Changing the key needs admin rights on this repository.';
  const base = repo.url.endsWith('/') ? repo.url : `${repo.url}/`;
  const gpgUrl = `${base}public.gpg`;
  const keyUrl = `${base}public.key`;

  const open = (d: 'rotate' | 'import') => {
    change.reset();
    setDone('');
    setDialog(d);
  };
  // Nothing typed into the import dialog outlives it.
  const close = () => {
    setDialog(null);
    setPrivateKey('');
    setPassphrase('');
    setMissingKey(false);
  };
  const onDone = (what: string) => () => {
    close();
    setDone(what);
  };

  let body;
  if (key.isPending) {
    body = (
      <p className="meta" aria-busy="true">
        Loading signing key…
      </p>
    );
  } else if (key.isError && isStatus(key.error, 501)) {
    body = (
      <p className="note note--warn" role="note">
        Signing keys are stored encrypted with the server&apos;s secret key, and none is set. Set{' '}
        <code className="inline-code">HANGAR_SECRET_KEY</code> (security.secret_key) and restart Hangar; until then the signed metadata and the public
        key answer 501.
      </p>
    );
  } else if (key.isError) {
    body = <ErrorNote what="the signing key" error={key.error} onRetry={() => void key.refetch()} />;
  } else {
    const k = key.data;
    body = (
      <>
        <dl className="signing-key__facts">
          <dt>Fingerprint</dt>
          <dd className="signing-key__fp">
            <span className="signing-key__hex">{groupHex(k.fingerprint)}</span>
            <CopyButton text={k.fingerprint} label="Copy fingerprint" />
          </dd>
          <dt>Algorithm</dt>
          <dd>{ALGORITHMS[k.algorithm] ?? k.algorithm}</dd>
          <dt>Created</dt>
          <dd>{k.created_at ? shortDate(k.created_at) : '—'}</dd>
          <dt>Public key</dt>
          <dd className="signing-key__files">
            <span className="signing-key__file">
              <a href={gpgUrl} download className="link-strong">public.gpg</a>
              <CopyButton text={gpgUrl} label="Copy public.gpg URL" />
            </span>
            <span className="signing-key__file">
              <a href={keyUrl} download className="link-strong">public.key</a>
              <CopyButton text={keyUrl} label="Copy public.key URL" />
            </span>
            <span className="meta">{repo.format === 'rpm' ? 'public.key is the gpgkey of the .repo file' : 'public.gpg goes in Signed-By'}</span>
          </dd>
        </dl>
        {!canChange && <p className="meta">{reason}</p>}
        <span className="visually-hidden" aria-live="polite">
          {done}
        </span>
      </>
    );
  }

  const actions = key.isSuccess && (
    <>
      <button type="button" className="button button--secondary button--sm" disabled={!canChange} title={canChange ? undefined : reason} onClick={() => open('rotate')}>
        Rotate…
      </button>
      <button type="button" className="button button--secondary button--sm" disabled={!canChange} title={canChange ? undefined : reason} onClick={() => open('import')}>
        Import key…
      </button>
    </>
  );

  return (
    <Panel
      title="Signing key"
      titleId="signing-title"
      className="signing-key"
      action={
        <span className="signing-key__actions">
          <Pill tone={shared ? 'violet' : 'info'}>{shared ? 'Shared key' : 'Own key'}</Pill>
          {actions}
        </span>
      }
    >
      {body}

      {dialog === 'rotate' && (
        <ConfirmDialog
          title={shared ? 'Rotate the shared signing key?' : `Rotate the signing key of ${repo.name}?`}
          confirmLabel="Rotate key"
          tone="danger"
          busy={change.isPending}
          error={change.isError ? changeError(change.error) : null}
          onCancel={close}
          onConfirm={() => change.mutate({ kind: 'rotate' }, { onSuccess: onDone('Signing key rotated') })}
        >
          <p>
            A new key signs the metadata at once; the old one is not kept for an overlap. Clients fail (apt: NO_PUBKEY) until they fetch the new{' '}
            <code className="inline-code">public.gpg</code> or <code className="inline-code">public.key</code>.
          </p>
          {shared && <p>Every repository that uses the shared key switches to it.</p>}
        </ConfirmDialog>
      )}

      {dialog === 'import' && (
        <ConfirmDialog
          title={shared ? 'Import the shared signing key' : `Import a signing key for ${repo.name}`}
          confirmLabel="Import key"
          busy={change.isPending}
          error={missingKey ? 'Paste the private key.' : change.isError ? changeError(change.error) : null}
          onCancel={close}
          onConfirm={() => {
            if (!privateKey.trim()) {
              setMissingKey(true);
              return;
            }
            change.mutate(
              { kind: 'import', private_key: privateKey, passphrase: passphrase || undefined },
              { onSuccess: onDone('Signing key imported') },
            );
          }}
        >
          <p>The key replaces the current one at once; clients must fetch the new public key. It is stored encrypted and never shown again.</p>
          <div className="field">
            <label htmlFor="signing-private-key" className="field__label">Private key</label>
            <textarea
              id="signing-private-key"
              className="input input--mono textarea"
              rows={5}
              spellCheck={false}
              autoComplete="off"
              placeholder="-----BEGIN PGP PRIVATE KEY BLOCK-----"
              value={privateKey}
              onChange={(e) => {
                setMissingKey(false);
                change.reset();
                setPrivateKey(e.target.value);
              }}
            />
            <span className="field__hint">ASCII-armored, e.g. from gpg --armor --export-secret-keys</span>
          </div>
          <div className="field">
            <label htmlFor="signing-passphrase" className="field__label">
              Passphrase <span className="field__optional">optional</span>
            </label>
            <input
              id="signing-passphrase"
              className="input"
              type="password"
              autoComplete="off"
              value={passphrase}
              onChange={(e) => {
                change.reset();
                setPassphrase(e.target.value);
              }}
            />
          </div>
        </ConfirmDialog>
      )}
    </Panel>
  );
}
