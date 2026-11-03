import { createContext, useContext } from 'react';

export interface CommandPaletteApi {
  open: () => void;
  close: () => void;
  isOpen: boolean;
}

export const CommandPaletteContext = createContext<CommandPaletteApi | null>(null);

export function useCommandPalette(): CommandPaletteApi {
  const ctx = useContext(CommandPaletteContext);
  if (!ctx) throw new Error('useCommandPalette must be used inside <CommandPaletteProvider>');
  return ctx;
}

export const isMac = () =>
  typeof navigator !== 'undefined' && /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent);
