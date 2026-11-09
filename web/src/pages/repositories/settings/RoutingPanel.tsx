/**
 * Routing rules of a proxy or group repository: the guard against
 * dependency confusion. Proxies get a block list (and an optional allow
 * list); groups get per-member patterns. Saved on their own
 * (PUT /repositories/{name}/routing), with a "test a name" check.
 */
import { useState, type KeyboardEvent } from 'react';
import { ApiRequestError } from '../../../api/client';
import { useSaveRouting, useSuggestRouting, useTestRouting } from '../../../api/catalog';
import { usePermissions } from '../../../api/permissions';
import type { Repository } from '../../../api/types';
import { Panel } from '../../../components/Panel';
import { Pill } from '../../../components/Pill';
import { TYPE_META } from '../../../lib/repositories';
import { blockReason, EXAMPLES, parsePatterns, showPatterns, toDraft, toRules, type Draft } from '../../../lib/routing';

function apiMessage(err: unknown, action: string): string {
  if (err instanceof ApiRequestError) {
    if (err.status === 403) return `You do not have permission to ${action}.`;
    return err.body?.message ?? `The server could not ${action}.`;
  }
  return 'Cannot reach the server. Check your connection and try again.';
}

/** Enter inside the panel acts on the panel, never on the settings form around it. */
const onEnter = (fn: () => void) => (e: KeyboardEvent<HTMLInputElement>) => {
  if (e.key === 'Enter') {
    e.preventDefault();
    fn();
  }
};

export function RoutingPanel({ repo, memberTypes }: { repo: Repository; memberTypes: ReadonlyMap<string, string> }) {
  const canAdmin = usePermissions().canAdminRepo(repo.name);
  const [draft, setDraft] = useState<Draft>(() => toDraft(repo));
  const [saved, setSaved] = useState(false);
  const [suggested, setSuggested] = useState<string | null>(null);
  const [testName, setTestName] = useState('');
  const save = useSaveRouting(repo.name);
  const suggest = useSuggestRouting(repo.name);
  const test = useTestRouting(repo.name);

  const stored = JSON.stringify(toRules(repo, toDraft(repo)));
  const dirty = JSON.stringify(toRules(repo, draft)) !== stored;
  const example = EXAMPLES[repo.format] ?? 'internal-*';
  const members = repo.config.group?.members ?? [];
  const isGroup = repo.type === 'group';

  const edit = (fn: (d: Draft) => Draft) => {
    if (save.isError) save.reset();
    setSaved(false);
    setDraft(fn);
  };
  const setMember = (m: string, key: 'exclusive' | 'block', value: string) =>
    edit((d) => ({ ...d, members: { ...d.members, [m]: { ...(d.members[m] ?? { exclusive: '', block: '' }), [key]: value } } }));

  const onSave = () => {
    if (!dirty || save.isPending) return;
    save.mutate(toRules(repo, draft), {
      onSuccess: () => setSaved(true),
    });
  };
  const runTest = () => {
    const n = testName.trim();
    if (n) test.mutate(n);
  };
  const onSuggest = () =>
    suggest.mutate(undefined, {
      onSuccess: (res) => {
        if (!res.suggestions.length) {
          setSuggested('Your hosted repositories publish nothing yet, so there is nothing to suggest.');
          return;
        }
        const added = new Set<string>();
        edit((d) => {
          const next: Draft = { ...d, members: { ...d.members } };
          for (const s of res.suggestions) {
            if (isGroup && s.member) {
              const cur = next.members[s.member] ?? { exclusive: '', block: '' };
              const ps = parsePatterns(cur.exclusive);
              if (!ps.includes(s.pattern)) {
                next.members[s.member] = { ...cur, exclusive: showPatterns([...ps, s.pattern]) };
                added.add(s.pattern);
              }
            } else if (!isGroup) {
              const ps = parsePatterns(next.block);
              if (!ps.includes(s.pattern)) {
                next.block = showPatterns([...ps, s.pattern]);
                added.add(s.pattern);
              }
            }
          }
          return next;
        });
        setSuggested(
          added.size
            ? `Added ${[...added].join(', ')}. Review and save.`
            : 'The suggested patterns are already in place.',
        );
      },
    });

  const patternInput = (id: string, label: string, value: string, onChange: (v: string) => void) => (
    <input
      id={id}
      className="input input--mono"
      type="text"
      aria-label={label}
      placeholder={example}
      value={value}
      disabled={!canAdmin}
      spellCheck={false}
      autoComplete="off"
      onChange={(e) => onChange(e.target.value)}
      onKeyDown={onEnter(onSave)}
    />
  );

  return (
    <Panel title="Routing rules" titleId="routing-title" className="routing">
      <p className="routing__intro">
        A public registry can publish your internal package names with a higher version (dependency confusion).{' '}
        {isGroup ? 'Reserve internal names for the member that owns them.' : 'Block the names your hosted repositories publish, so they are never asked for upstream.'}
      </p>
      {canAdmin && (
        <div className="routing__actions">
          <button type="button" className="button button--secondary" disabled={suggest.isPending} onClick={onSuggest}>
            {suggest.isPending ? 'Looking at hosted names…' : 'Block names your hosted repositories publish'}
          </button>
          {suggested && <span className="field__hint" role="status">{suggested}</span>}
          {suggest.isError && (
            <span className="form-error" role="alert">
              {apiMessage(suggest.error, 'suggest patterns')}
            </span>
          )}
        </div>
      )}

      {isGroup ? (
        <div className="table-scroll">
          <table className="data-table routing__table">
            <thead>
              <tr>
                <th scope="col">Member</th>
                <th scope="col">Only from this member</th>
                <th scope="col">Never from this member</th>
              </tr>
            </thead>
            <tbody>
              {members.map((m) => {
                const type = memberTypes.get(m) as keyof typeof TYPE_META | undefined;
                return (
                  <tr key={m}>
                    <th scope="row" className="routing__member">
                      <span>{m}</span>
                      {type && TYPE_META[type] && <Pill tone={TYPE_META[type].tone}>{TYPE_META[type].label}</Pill>}
                    </th>
                    <td data-label="Only from this member">{patternInput(`routing-${m}-exclusive`, `Names only ${m} serves`, draft.members[m]?.exclusive ?? '', (v) => setMember(m, 'exclusive', v))}</td>
                    <td data-label="Never from this member">{patternInput(`routing-${m}-block`, `Names ${m} never serves`, draft.members[m]?.block ?? '', (v) => setMember(m, 'block', v))}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      ) : (
        <div className="routing__fields">
          <div className="field">
            <label htmlFor="routing-block" className="field__label">Block</label>
            {patternInput('routing-block', 'Block', draft.block, (v) => edit((d) => ({ ...d, block: v })))}
            <span className="field__hint">Never fetched from the remote nor served from the cache.</span>
          </div>
          <div className="field">
            <label htmlFor="routing-allow" className="field__label">
              Allow only <span className="field__optional">optional</span>
            </label>
            {patternInput('routing-allow', 'Allow only', draft.allow, (v) => edit((d) => ({ ...d, allow: v })))}
            <span className="field__hint">When set, every other name is blocked.</span>
          </div>
        </div>
      )}
      <p className="field__hint">
        Comma-separated patterns; <code className="inline-code">*</code> matches anything.
        {repo.format === 'go' && ' Module path prefixes, as in GOPRIVATE.'}
        {repo.format === 'maven' && ' groupId, or groupId:artifactId.'}
      </p>

      {save.isError && (
        <p className="form-error" role="alert">
          {apiMessage(save.error, 'save the routing rules')}
        </p>
      )}
      <p className="visually-hidden" aria-live="polite">
        {saved && !dirty ? 'Routing rules saved' : ''}
      </p>
      {canAdmin && (
        <div className="routing__actions">
          <button type="button" className="button button--primary" disabled={!dirty || save.isPending} onClick={onSave}>
            {save.isPending ? 'Saving…' : saved && !dirty ? 'Saved' : 'Save routing rules'}
          </button>
          {dirty && (
            <button
              type="button"
              className="button button--secondary"
              onClick={() => {
                save.reset();
                setSuggested(null);
                setDraft(toDraft(repo));
              }}
            >
              Discard
            </button>
          )}
        </div>
      )}

      <div className="routing__test">
        <label htmlFor="routing-test" className="field__label">Test a name</label>
        <div className="routing__test-row">
          <input
            id="routing-test"
            className="input input--mono"
            type="text"
            placeholder={example.replace('*', 'core')}
            value={testName}
            spellCheck={false}
            autoComplete="off"
            onChange={(e) => {
              if (test.data || test.isError) test.reset();
              setTestName(e.target.value);
            }}
            onKeyDown={onEnter(runTest)}
          />
          <button type="button" className="button button--secondary" disabled={!testName.trim() || test.isPending} onClick={runTest}>
            {test.isPending ? 'Testing…' : 'Test'}
          </button>
        </div>
        {dirty && <span className="field__hint">Tests use the saved rules.</span>}
        {test.isError && (
          <p className="form-error" role="alert">
            {apiMessage(test.error, 'test the name')}
          </p>
        )}
        {test.data && (
          <div className="routing__result" role="status" aria-label="Routing test result">
            <p className="routing__verdict">
              {test.data.allowed
                ? isGroup
                  ? test.data.served_by
                    ? `Served by ${test.data.served_by}`
                    : 'Allowed'
                  : `${repo.name} may fetch and serve it`
                : `Blocked: ${isGroup ? 'no member may serve it' : blockReason(test.data.block!)}`}
              {test.data.normalized !== test.data.name && <span className="meta"> · compared as {test.data.normalized}</span>}
            </p>
            {isGroup && (
              <ul className="routing__hops">
                {test.data.members.map((h) => (
                  <li key={[...h.via, h.repository].join('/')} className={h.allowed ? 'is-allowed' : 'is-blocked'}>
                    <span aria-hidden="true">{h.allowed ? '✓' : '✕'}</span> {[...h.via.slice(1), h.repository].join(' › ')}
                    <span className="meta">
                      {h.allowed ? (h.online ? ' may serve it' : ' allowed, but offline') : ` excluded: ${blockReason(h.block!)}`}
                    </span>
                  </li>
                ))}
              </ul>
            )}
          </div>
        )}
      </div>
    </Panel>
  );
}
