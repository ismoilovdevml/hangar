/**
 * Format-specific settings (config.format and, for Helm proxies,
 * config.proxy.related_hosts), shown only for the format and type they apply to.
 */
import { Select } from '../../components/Select';
import {
  APT_DEFAULT_ARCHITECTURES,
  formatSettingKeys,
  RELATED_HOSTS_FORMATS,
  RPM_MAX_REPODATA_DEPTH,
  type RepoForm,
  type RepoFormErrors,
  type RepoFormField,
  type SigningKeyChoice,
  type SumdbVia,
} from '../../lib/repositories';
import { ListInput } from '../settings/controls';
import { FieldError, type Setter } from './RepoFormFields';
import { errorId, fieldId, invalidProps } from './repoFormHelpers';

const SIGNING_KEYS: { value: SigningKeyChoice; label: string; hint: string }[] = [
  { value: 'repository', label: 'Own key for this repository', hint: 'Generated on first use' },
  { value: 'shared', label: 'Shared server key', hint: 'One key for many repositories' },
];

const SUMDB_VIA: { value: SumdbVia; label: string; hint: string }[] = [
  { value: 'database', label: 'Directly to the database', hint: 'https://<name>/…' },
  { value: 'remote', label: 'Through the remote', hint: '<remote URL>/sumdb/<name>/…' },
];

const describedBy = (field: RepoFormField, errors: RepoFormErrors, hint: string) => (errors[field] ? errorId(field) : hint);

export function FormatSettings({ form, set, errors, blur }: { form: RepoForm; set: Setter; errors: RepoFormErrors; blur: (f: RepoFormField) => () => void }) {
  const keys = formatSettingKeys(form.format, form.type);
  const relatedHosts = form.type === 'proxy' && !!form.format && RELATED_HOSTS_FORMATS.includes(form.format);
  if (keys.length === 0 && !relatedHosts) return null;

  return (
    <>
      {keys.includes('architectures') && (
        <div className="field">
          <label htmlFor={fieldId('aptArchitectures')} className="field__label">
            Architectures <span className="field__optional">optional</span>
          </label>
          <ListInput
            id={fieldId('aptArchitectures')}
            values={form.aptArchitectures}
            onChange={(v) => {
              set('aptArchitectures', v);
              blur('aptArchitectures')();
            }}
            placeholder={APT_DEFAULT_ARCHITECTURES.join(', ')}
            itemLabel="architecture"
            invalid={!!errors.aptArchitectures}
            describedBy={describedBy('aptArchitectures', errors, 'repo-aptArchitectures-hint')}
          />
          {!errors.aptArchitectures && (
            <span id="repo-aptArchitectures-hint" className="field__hint">
              Indexed even before a package for them is uploaded. Empty: {APT_DEFAULT_ARCHITECTURES.join(' and ')}
            </span>
          )}
          <FieldError field="aptArchitectures" errors={errors} />
        </div>
      )}

      {keys.includes('repodata_depth') && (
        <div className="field">
          <label htmlFor={fieldId('rpmRepodataDepth')} className="field__label">Repodata depth</label>
          <input
            className="input input--mono"
            type="number"
            inputMode="numeric"
            min={0}
            max={RPM_MAX_REPODATA_DEPTH}
            step={1}
            value={form.rpmRepodataDepth}
            onChange={(e) => set('rpmRepodataDepth', e.target.value)}
            onBlur={blur('rpmRepodataDepth')}
            {...invalidProps('rpmRepodataDepth', errors, 'repo-rpmRepodataDepth-hint')}
          />
          <span id="repo-rpmRepodataDepth-hint" className="field__hint">
            0: one yum repository. 2: each <code className="inline-code">el9/x86_64/</code> gets its own repodata
          </span>
          <FieldError field="rpmRepodataDepth" errors={errors} />
        </div>
      )}

      {keys.includes('signing_key') && (
        <div className="field">
          <label htmlFor="repo-signingKey" className="field__label">Signing key</label>
          <Select<SigningKeyChoice>
            id="repo-signingKey"
            value={form.signingKey}
            options={SIGNING_KEYS}
            onChange={(v) => set('signingKey', v)}
            aria-describedby="repo-signingKey-hint"
          />
          <span id="repo-signingKey-hint" className="field__hint">Signs the metadata clients verify. The shared key is managed by system:repos admins</span>
        </div>
      )}

      {keys.includes('sumdbs') && (
        <div className="field">
          <label htmlFor={fieldId('goSumdbs')} className="field__label">Checksum databases</label>
          <ListInput
            id={fieldId('goSumdbs')}
            values={form.goSumdbs}
            onChange={(v) => {
              set('goSumdbs', v);
              blur('goSumdbs')();
            }}
            placeholder="sum.golang.org"
            itemLabel="checksum database"
            invalid={!!errors.goSumdbs}
            describedBy={describedBy('goSumdbs', errors, 'repo-goSumdbs-hint')}
          />
          {!errors.goSumdbs && (
            <span id="repo-goSumdbs-hint" className="field__hint">
              Passed through under sumdb/. Empty turns it off. Private modules belong in GOPRIVATE, which skips the database
            </span>
          )}
          <FieldError field="goSumdbs" errors={errors} />
        </div>
      )}

      {keys.includes('sumdb_via') && (
        <div className="field">
          <label htmlFor="repo-goSumdbVia" className="field__label">Checksum requests go</label>
          <Select<SumdbVia>
            id="repo-goSumdbVia"
            value={form.goSumdbVia}
            options={SUMDB_VIA}
            onChange={(v) => set('goSumdbVia', v)}
            aria-describedby="repo-goSumdbVia-hint"
          />
          <span id="repo-goSumdbVia-hint" className="field__hint">Through the remote only if it proxies the database (Athens, another Hangar)</span>
        </div>
      )}

      {keys.includes('download_url') && (
        <div className="field field--wide">
          <label htmlFor={fieldId('cargoDownloadUrl')} className="field__label">
            Crate download URL <span className="field__optional">optional</span>
          </label>
          <input
            className="input input--mono"
            type="text"
            autoComplete="off"
            spellCheck={false}
            placeholder="https://static.crates.io/crates/{crate}/{crate}-{version}.crate"
            value={form.cargoDownloadUrl}
            onChange={(e) => set('cargoDownloadUrl', e.target.value)}
            onBlur={blur('cargoDownloadUrl')}
            {...invalidProps('cargoDownloadUrl', errors, 'repo-cargoDownloadUrl-hint')}
          />
          <span id="repo-cargoDownloadUrl-hint" className="field__hint">
            Empty: the &quot;dl&quot; of the remote&apos;s config.json. Markers {'{crate}'}, {'{version}'}, {'{prefix}'}, {'{lowerprefix}'}, {'{sha256-checksum}'}
          </span>
          <FieldError field="cargoDownloadUrl" errors={errors} />
        </div>
      )}

      {relatedHosts && (
        <div className="field field--wide">
          <label htmlFor={fieldId('relatedHosts')} className="field__label">
            Chart hosts <span className="field__optional">optional</span>
          </label>
          <ListInput
            id={fieldId('relatedHosts')}
            values={form.relatedHosts}
            onChange={(v) => {
              set('relatedHosts', v);
              blur('relatedHosts')();
            }}
            placeholder="charts.example.com or .example.com"
            itemLabel="host"
            invalid={!!errors.relatedHosts}
            describedBy={describedBy('relatedHosts', errors, 'repo-relatedHosts-hint')}
          />
          {!errors.relatedHosts && (
            <span id="repo-relatedHosts-hint" className="field__hint">
              Other hosts the index links chart archives to. The remote&apos;s own host and github.com are always allowed; credentials are never sent here
            </span>
          )}
          <FieldError field="relatedHosts" errors={errors} />
        </div>
      )}
    </>
  );
}
