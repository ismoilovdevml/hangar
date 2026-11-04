import { useId, useState } from 'react';
import { useCanSystem } from '../../api/ops';
import { ErrorNote } from '../../components/ErrorNote';
import { PageHeader } from '../../components/PageHeader';
import { Panel } from '../../components/Panel';
import { Segmented } from '../../components/Segmented';
import { formatUtc } from '../../lib/tasks';
import { ldapForm, oidcForm, type LdapForm, type OidcForm } from './model';
import {
  PRESET_CARDS,
  applyLdapPreset,
  applyOidcPreset,
  inferLdapPreset,
  inferOidcPreset,
  ldapPreset,
  oidcPreset,
  protocolOf,
  type PresetId,
  type Protocol,
} from './presets';
import { LdapEditor, OidcEditor } from './providerEditors';
import { useIdentityProviders, useLdapSettings, useOidcSettings, type IdentityProvider, type LDAPSettings, type OIDCSettings } from './settingsApi';
import { NoPermission } from './NoPermission';
import './settings.css';

export function AuthenticationPage() {
  const canRead = useCanSystem('settings', 'read');
  const canEdit = useCanSystem('settings', 'admin');
  if (!canRead) return <NoPermission what="sign-in settings" />;
  return <Authentication canEdit={canEdit} />;
}

const stored = (s: { source: string } | undefined) => !!s && s.source !== 'default';

/** The card the page opens on: the provider in use, else one saved earlier, else the first. */
function initialPreset(oidc: OIDCSettings | undefined, ldap: LDAPSettings | undefined): PresetId {
  if (stored(oidc) && oidc!.enabled) return inferOidcPreset(oidc!);
  if (stored(ldap) && ldap!.enabled) return inferLdapPreset(ldap!);
  if (stored(oidc)) return inferOidcPreset(oidc!);
  if (stored(ldap)) return inferLdapPreset(ldap!);
  return PRESET_CARDS[0]!.id;
}

/** The protocol "Other" opens with: the one saved as "other", else OIDC. */
function otherProtocol(oidc: OIDCSettings | undefined, ldap: LDAPSettings | undefined): Protocol {
  if (stored(oidc) && inferOidcPreset(oidc!) === 'other') return 'oidc';
  if (stored(ldap) && inferLdapPreset(ldap!) === 'other') return 'ldap';
  return 'oidc';
}

/** Presets in use, by card, for the "in use" marks. */
function inUse(oidc: OIDCSettings | undefined, ldap: LDAPSettings | undefined): Set<PresetId> {
  const out = new Set<PresetId>();
  if (stored(oidc) && oidc!.enabled) out.add(inferOidcPreset(oidc!));
  if (stored(ldap) && ldap!.enabled) out.add(inferLdapPreset(ldap!));
  return out;
}

function Authentication({ canEdit }: { canEdit: boolean }) {
  const titleId = useId();
  const oidcQ = useOidcSettings();
  const ldapQ = useLdapSettings();
  const oidc = oidcQ.data;
  const ldap = ldapQ.data;
  const settled = !oidcQ.isPending && !ldapQ.isPending;

  const [picked, setPicked] = useState<PresetId | null>(null);
  const [pickedOther, setPickedOther] = useState<Protocol | null>(null);
  const [oidcDraft, setOidcDraft] = useState<OidcForm | null>(null);
  const [ldapDraft, setLdapDraft] = useState<LdapForm | null>(null);

  const selected = picked ?? (settled ? initialPreset(oidc, ldap) : null);
  const otherProto = pickedOther ?? otherProtocol(oidc, ldap);
  const protocol: Protocol | null = selected ? (protocolOf(selected) ?? otherProto) : null;

  // A provider never saved starts from its preset; a saved one from what is stored.
  const freshOidc = (s: OIDCSettings, id: PresetId) => (stored(s) ? oidcForm(s) : applyOidcPreset(oidcForm(s), oidcPreset(id), s));
  const freshLdap = (s: LDAPSettings, id: PresetId) => (stored(s) ? ldapForm(s) : applyLdapPreset(ldapForm(s), ldapPreset(id), s));
  const oidcId: PresetId = protocol === 'oidc' && selected ? selected : stored(oidc) ? inferOidcPreset(oidc!) : 'keycloak';
  const ldapId: PresetId = protocol === 'ldap' && selected ? selected : stored(ldap) ? inferLdapPreset(ldap!) : 'active-directory';
  const oidcValue = oidcDraft ?? (oidc ? freshOidc(oidc, oidcId) : null);
  const ldapValue = ldapDraft ?? (ldap ? freshLdap(ldap, ldapId) : null);
  const editOidc = (fn: (f: OidcForm) => OidcForm) => setOidcDraft((d) => fn(d ?? freshOidc(oidc!, oidcId)));
  const editLdap = (fn: (f: LdapForm) => LdapForm) => setLdapDraft((d) => fn(d ?? freshLdap(ldap!, ldapId)));

  /** Fills the form of the preset's protocol, unless the configuration file or permissions keep it as it is. */
  const switchTo = (id: PresetId, proto: Protocol) => {
    if (!canEdit) return;
    if (proto === 'oidc' && oidc && oidc.source !== 'file') editOidc((f) => applyOidcPreset(f, oidcPreset(id), oidc));
    if (proto === 'ldap' && ldap && ldap.source !== 'file') editLdap((f) => applyLdapPreset(f, ldapPreset(id), ldap));
  };
  const pick = (id: PresetId) => {
    setPicked(id);
    switchTo(id, protocolOf(id) ?? otherProto);
  };
  const pickOther = (proto: Protocol) => {
    setPickedOther(proto);
    switchTo('other', proto);
  };

  const protocolSwitch =
    selected === 'other' ? (
      <Segmented
        label="Kind of provider"
        className="auth-editor__kind"
        value={otherProto}
        onChange={pickOther}
        options={[
          { value: 'oidc', label: 'OpenID Connect' },
          { value: 'ldap', label: 'LDAP directory' },
        ]}
      />
    ) : undefined;

  let editor;
  if (!selected || (protocol === 'oidc' && oidcQ.isPending) || (protocol === 'ldap' && ldapQ.isPending)) {
    editor = (
      <Panel className="auth-editor" aria-busy="true" aria-label="Sign-in method">
        <p className="meta">Loading the sign-in settings…</p>
      </Panel>
    );
  } else if (protocol === 'oidc') {
    editor =
      oidc && oidcValue ? (
        <OidcEditor
          key={`oidc-${selected}`}
          titleId={titleId}
          preset={oidcPreset(selected)}
          data={oidc}
          form={oidcValue}
          setForm={editOidc}
          canEdit={canEdit}
          protocolSwitch={protocolSwitch}
        />
      ) : (
        <Panel className="auth-editor" aria-label="Single sign-on">
          {protocolSwitch}
          <ErrorNote what="the single sign-on settings" onRetry={() => void oidcQ.refetch()} />
        </Panel>
      );
  } else {
    editor =
      ldap && ldapValue ? (
        <LdapEditor
          key={`ldap-${selected}`}
          titleId={titleId}
          preset={ldapPreset(selected)}
          data={ldap}
          form={ldapValue}
          setForm={editLdap}
          canEdit={canEdit}
          protocolSwitch={protocolSwitch}
        />
      ) : (
        <Panel className="auth-editor" aria-label="Directory sign-in">
          {protocolSwitch}
          <ErrorNote what="the LDAP settings" onRetry={() => void ldapQ.refetch()} />
        </Panel>
      );
  }

  return (
    <div className="screen settings-screen auth-screen">
      <PageHeader title="Authentication" actions={<ProviderChips oidc={oidc} ldap={ldap} />} />
      <ProviderProblems oidc={oidc} ldap={ldap} />
      <PresetPicker selected={selected} inUse={inUse(oidc, ldap)} onPick={pick} />
      {editor}
    </div>
  );
}

/* ---------------------------------------------------------------- status */

function providerLabel(p: IdentityProvider, oidc: OIDCSettings | undefined, ldap: LDAPSettings | undefined): string {
  if (p.kind === 'oidc') return oidc?.display_name || 'Single sign-on';
  if (p.kind === 'ldap' && ldap) {
    const id = inferLdapPreset(ldap);
    return id === 'other' ? 'LDAP directory' : ldapPreset(id).name;
  }
  return p.name;
}

const STATE_TEXT = { ok: 'working', error: 'failing', disabled: 'off' } as const;

/** The sign-in methods that are on, as small status chips next to the title. */
function ProviderChips({ oidc, ldap }: { oidc?: OIDCSettings; ldap?: LDAPSettings }) {
  const q = useIdentityProviders();
  const on = (q.data?.items ?? []).filter((p) => p.enabled);
  return (
    <ul className="auth-chips" aria-label="Sign-in methods" aria-busy={q.isPending || undefined}>
      {on.map((p) => (
        <li key={p.name} className="auth-chip glass" title={p.last_sync_at ? `Last request ${formatUtc(p.last_sync_at)}` : undefined}>
          <span className={`status__dot dot-${p.state === 'ok' ? 'ok' : 'bad'}`} aria-hidden="true" />
          {providerLabel(p, oidc, ldap)}
          {p.state === 'ok' ? <span className="visually-hidden"> · {STATE_TEXT.ok}</span> : <span className="auth-chip__state"> · {STATE_TEXT[p.state]}</span>}
        </li>
      ))}
      <li className="auth-chip glass">
        <span className="status__dot dot-ok" aria-hidden="true" />
        Local accounts<span className="visually-hidden"> · always on</span>
      </li>
    </ul>
  );
}

/** Spells out why a provider that is on does not work. */
function ProviderProblems({ oidc, ldap }: { oidc?: OIDCSettings; ldap?: LDAPSettings }) {
  const q = useIdentityProviders();
  if (q.isError) return <ErrorNote what="the sign-in status" onRetry={() => void q.refetch()} />;
  const failing = (q.data?.items ?? []).filter((p) => p.enabled && p.state === 'error');
  if (!failing.length) return null;
  return (
    <div className="auth-problems" role="alert">
      {failing.map((p) => (
        <p key={p.name}>
          <strong>{providerLabel(p, oidc, ldap)} is failing:</strong> {p.error ?? 'unknown error'}
          {p.last_sync_at && <span className="meta"> · last request {formatUtc(p.last_sync_at)}</span>}
        </p>
      ))}
    </div>
  );
}

/* ---------------------------------------------------------------- picker */

function PresetPicker({ selected, inUse, onPick }: { selected: PresetId | null; inUse: Set<PresetId>; onPick: (id: PresetId) => void }) {
  const id = useId();
  return (
    <Panel className="auth-picker" aria-labelledby={id}>
      <h2 id={id} className="auth-picker__title">
        Add a sign-in method · what do you use?
      </h2>
      <div className="auth-picker__grid">
        {PRESET_CARDS.map((c) => (
          <button key={c.id} type="button" className="auth-preset" aria-pressed={selected === c.id} onClick={() => onPick(c.id)}>
            <span className="auth-preset__name">{c.name}</span>
            <span className="auth-preset__kind">
              {c.kind}
              {inUse.has(c.id) && (
                <span className="auth-preset__used">
                  <span className="status__dot dot-ok" aria-hidden="true" /> in use
                </span>
              )}
            </span>
          </button>
        ))}
      </div>
    </Panel>
  );
}
