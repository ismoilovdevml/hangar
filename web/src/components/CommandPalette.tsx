import { useCallback, useEffect, useId, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from 'react';
import { createPortal } from 'react-dom';
import { useNavigate } from '@tanstack/react-router';
import { useSession } from '../api/hooks';
import { GATES, passes } from '../lib/gates';
import { can } from '../lib/privileges';
import { canSee, NAV, SETTINGS_PAGES } from '../nav';
import { CommandPaletteContext } from './commandPaletteContext';
import { IconArrowRight, IconDatabase, IconKey, IconLogout, IconPackage, IconPlus, IconSearch, IconShield, IconUser } from './icons';
import { usePaletteEntities } from './paletteEntities';

interface Command {
  id: string;
  group: 'Go to' | 'Repositories' | 'Packages' | 'Vulnerabilities' | 'Users' | 'Actions';
  label: string;
  /** Muted text after the label (format, repository, severity). */
  hint?: string;
  keywords?: string;
  icon: ReactNode;
  run: () => void;
}

/** 0: the label starts with q, 1: contains it, 2: only the keywords match, -1: no match. */
function rank(c: Command, q: string): number {
  const label = c.label.toLowerCase();
  if (label.startsWith(q)) return 0;
  if (label.includes(q)) return 1;
  return (c.keywords ?? '').toLowerCase().includes(q) ? 2 : -1;
}

interface ProviderProps {
  children: ReactNode;
  onSignOut?: () => void;
}

export function CommandPaletteProvider({ children, onSignOut }: ProviderProps) {
  const [isOpen, setOpen] = useState(false);
  const open = useCallback(() => setOpen(true), []);
  const close = useCallback(() => setOpen(false), []);

  useEffect(() => {
    const onKey = (e: globalThis.KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && !e.altKey && e.key.toLowerCase() === 'k') {
        e.preventDefault();
        // Another modal (a dialog, a drawer) owns the keyboard: opening behind it would steal its focus.
        if (document.querySelector('[aria-modal="true"]:not(.palette)')) return;
        setOpen((v) => !v);
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);

  const api = useMemo(() => ({ open, close, isOpen }), [open, close, isOpen]);
  return (
    <CommandPaletteContext.Provider value={api}>
      {children}
      {isOpen && <CommandPaletteDialog onClose={close} onSignOut={onSignOut} />}
    </CommandPaletteContext.Provider>
  );
}

function CommandPaletteDialog({ onClose, onSignOut }: { onClose: () => void; onSignOut?: () => void }) {
  const navigate = useNavigate();
  const privileges = useSession().data?.privileges;
  const [query, setQuery] = useState('');
  const [active, setActive] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);
  const listId = useId();
  const titleId = useId();

  useEffect(() => {
    const previous = document.activeElement as HTMLElement | null;
    inputRef.current?.focus();
    return () => previous?.focus?.();
  }, []);

  const commands = useMemo<Command[]>(() => {
    // Sidebar pages first, then the pages that live under Settings.
    const go: Command[] = [...NAV, SETTINGS_PAGES]
      .filter((section) => canSee(privileges, section.requires))
      .flatMap((section) =>
        section.items
          .filter((item) => canSee(privileges, item.requires))
          .map((item) => ({
            id: `nav:${item.to}${item.hash ? `#${item.hash}` : ''}`,
            group: 'Go to' as const,
            label: item.label,
            keywords: `${section.title} ${item.keywords ?? ''}`,
            icon: <item.icon size={16} />,
            run: () => void navigate({ to: item.to, hash: item.hash }),
          })),
      );
    const actions: Command[] = [];
    if (passes(GATES.createRepository, privileges)) {
      actions.push({
        id: 'action:new-repo',
        group: 'Actions',
        label: 'Create a repository',
        keywords: 'new add hosted proxy group',
        icon: <IconPlus size={16} />,
        run: () => void navigate({ to: '/repositories/new' }),
      });
    }
    actions.push({
      id: 'action:new-token',
      group: 'Actions',
      label: 'Create a token',
      keywords: 'new access token api ci cli',
      icon: <IconKey size={16} />,
      run: () => void navigate({ to: '/account', hash: 'my-tokens-title' }),
    });
    if (onSignOut) {
      actions.push({ id: 'action:sign-out', group: 'Actions', label: 'Sign out', keywords: 'logout log out', icon: <IconLogout size={16} />, run: onSignOut });
    }
    return [...go, ...actions];
  }, [navigate, onSignOut, privileges]);

  const entities = usePaletteEntities(query, { users: !privileges || can(privileges, 'read', 'system', 'users') });

  const found = useMemo<Command[]>(() => {
    if (!entities.query) return [];
    return [
      ...entities.repositories.map<Command>((r) => ({
        id: `repo:${r.name}`,
        group: 'Repositories',
        label: r.name,
        hint: `${r.format} · ${r.type}`,
        icon: <IconDatabase size={16} />,
        run: () => void navigate({ to: '/repositories/$name/settings', params: { name: r.name } }),
      })),
      ...entities.packages.map<Command>((h) => ({
        id: `pkg:${h.component_id}`,
        group: 'Packages',
        label: h.coordinates,
        hint: h.repository,
        icon: <IconPackage size={16} />,
        run: () =>
          void navigate({
            to: '/browse',
            search: { repo: h.repository, ns: h.namespace || undefined, name: h.name, version: h.version || undefined },
          }),
      })),
      ...entities.findings.map<Command>((f) => ({
        id: `finding:${f.id}`,
        group: 'Vulnerabilities',
        label: `${f.advisory_id} · ${f.installed ? `${f.package}@${f.installed}` : f.package}`,
        hint: f.severity,
        icon: <IconShield size={16} />,
        run: () => void navigate({ to: '/vulnerabilities', search: { finding: f.id } }),
      })),
      ...entities.users.map<Command>((u) => ({
        id: `user:${u.id}`,
        group: 'Users',
        label: u.username,
        hint: u.display_name && u.display_name !== u.username ? u.display_name : undefined,
        icon: <IconUser size={16} />,
        run: () => void navigate({ to: '/access', search: { user: u.id } }),
      })),
    ];
  }, [entities, navigate]);

  const results = useMemo<Command[]>(() => {
    const q = query.trim().toLowerCase();
    if (!q) return commands;
    // Pages whose name matches come first, then live results and actions;
    // a package search is the fallback at the end.
    const ranked = commands
      .map((c) => ({ c, r: rank(c, q) }))
      .filter((x) => x.r >= 0)
      .sort((a, b) => a.r - b.r);
    const pages = ranked.filter((x) => x.c.group === 'Go to').map((x) => x.c);
    const actions = ranked.filter((x) => x.c.group === 'Actions').map((x) => x.c);
    const search: Command = {
      id: 'search',
      group: 'Actions',
      label: `Search packages for “${query.trim()}”`,
      icon: <IconSearch size={16} />,
      run: () => void navigate({ to: '/search', search: { q: query.trim() } }),
    };
    return [...pages, ...found, ...actions, search];
  }, [commands, found, query, navigate]);

  const safeActive = results.length ? Math.min(active, results.length - 1) : -1;

  const runAt = (i: number) => {
    const cmd = results[i];
    if (!cmd) return;
    onClose();
    cmd.run();
  };

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    switch (e.key) {
      case 'Escape':
        e.preventDefault();
        onClose();
        break;
      case 'ArrowDown':
        e.preventDefault();
        setActive((safeActive + 1) % Math.max(1, results.length));
        break;
      case 'ArrowUp':
        // Stops at the first option: wrapping would land on "Sign out".
        e.preventDefault();
        setActive(Math.max(0, safeActive - 1));
        break;
      case 'Home':
        e.preventDefault();
        setActive(0);
        break;
      case 'End':
        e.preventDefault();
        setActive(results.length - 1);
        break;
      case 'Enter':
        e.preventDefault();
        runAt(safeActive);
        break;
      case 'Tab':
        // Focus stays on the input; options are reached with the arrow keys.
        e.preventDefault();
        break;
    }
  };

  useEffect(() => {
    if (safeActive < 0) return;
    document.getElementById(`${listId}-${safeActive}`)?.scrollIntoView?.({ block: 'nearest' });
  }, [safeActive, listId]);

  let lastGroup: string | null = null;

  return createPortal(
    <div className="palette-scrim" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div role="dialog" aria-modal="true" aria-labelledby={titleId} className="palette glass" onKeyDown={onKeyDown}>
        <h2 id={titleId} className="visually-hidden">
          Command palette
        </h2>
        <div className="palette__search">
          <IconSearch size={18} />
          <input
            ref={inputRef}
            type="text"
            role="combobox"
            aria-expanded="true"
            aria-controls={listId}
            aria-autocomplete="list"
            aria-activedescendant={safeActive >= 0 ? `${listId}-${safeActive}` : undefined}
            aria-label="Search commands, pages and packages"
            placeholder="Jump to a page, run an action, or search packages…"
            value={query}
            onChange={(e) => {
              setQuery(e.target.value);
              setActive(0);
            }}
            autoComplete="off"
            spellCheck={false}
          />
          <kbd className="kbd">Esc</kbd>
        </div>
        <ul id={listId} role="listbox" aria-label="Results" className="palette__list">
          {results.length === 0 && <li className="palette__empty">No matching commands.</li>}
          {results.map((c, i) => {
            const header = c.group !== lastGroup ? c.group : null;
            lastGroup = c.group;
            return (
              <li key={c.id} role="presentation">
                {header && (
                  <div className="palette__group" aria-hidden="true">
                    {header}
                  </div>
                )}
                <div
                  id={`${listId}-${i}`}
                  role="option"
                  aria-selected={i === safeActive}
                  className={i === safeActive ? 'palette__option is-active' : 'palette__option'}
                  onMouseMove={() => i !== safeActive && setActive(i)}
                  onClick={() => runAt(i)}
                >
                  <span className="palette__icon">{c.icon}</span>
                  <span className="palette__label">{c.label}</span>
                  {c.hint && <span className="palette__hint">{c.hint}</span>}
                  {i === safeActive && <IconArrowRight size={14} />}
                </div>
              </li>
            );
          })}
        </ul>
        <div className="palette__foot" aria-hidden="true">
          <span><kbd className="kbd">↑</kbd><kbd className="kbd">↓</kbd> move</span>
          <span><kbd className="kbd">↵</kbd> open</span>
          <span><kbd className="kbd">Esc</kbd> close</span>
        </div>
      </div>
    </div>,
    document.body,
  );
}
