import { useCallback, useEffect, useId, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from 'react';
import { createPortal } from 'react-dom';
import { useNavigate } from '@tanstack/react-router';
import { useSession } from '../api/hooks';
import { allows } from '../lib/can';
import { NAV } from '../nav';
import { CommandPaletteContext } from './commandPaletteContext';
import { IconArrowRight, IconLogout, IconPlus, IconSearch } from './icons';

interface Command {
  id: string;
  group: 'Go to' | 'Actions';
  label: string;
  keywords?: string;
  icon: ReactNode;
  run: () => void;
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
    const go: Command[] = NAV.filter(
      (section) => !section.requires || allows(privileges, 'system', section.requires.area, section.requires.action),
    ).flatMap((section) =>
      section.items.map((item) => ({
        id: `nav:${item.to}`,
        group: 'Go to' as const,
        label: item.label,
        keywords: `${section.title} ${item.keywords ?? ''}`,
        icon: <item.icon size={16} />,
        run: () => void navigate({ to: item.to }),
      })),
    );
    const actions: Command[] = [
      {
        id: 'action:new-repo',
        group: 'Actions',
        label: 'Create a repository',
        keywords: 'new add hosted proxy group',
        icon: <IconPlus size={16} />,
        run: () => void navigate({ to: '/repositories/new' }),
      },
    ];
    if (onSignOut) {
      actions.push({ id: 'action:sign-out', group: 'Actions', label: 'Sign out', keywords: 'logout log out', icon: <IconLogout size={16} />, run: onSignOut });
    }
    return [...go, ...actions];
  }, [navigate, onSignOut, privileges]);

  const results = useMemo<Command[]>(() => {
    const q = query.trim().toLowerCase();
    const matched = q
      ? commands.filter((c) => `${c.label} ${c.keywords ?? ''}`.toLowerCase().includes(q))
      : commands;
    if (!q) return matched;
    const search: Command = {
      id: 'search',
      group: 'Actions',
      label: `Search packages for “${query.trim()}”`,
      icon: <IconSearch size={16} />,
      run: () => void navigate({ to: '/search', search: { q: query.trim() } }),
    };
    return [search, ...matched];
  }, [commands, query, navigate]);

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
        e.preventDefault();
        setActive((safeActive - 1 + results.length) % Math.max(1, results.length));
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
