import React from 'react';
import { createRoot } from 'react-dom/client';
import { HashRouter } from 'react-router-dom';
import App from './App';
import { installSelfHealHandlers } from './spaSelfHealClient';
import { ModalProvider } from './components/ui/ModalContext';
import { install as installLogBuffer } from './services/logBufferService';
import './styles/index.css';
// Pride scheme rainbow flourishes — scoped to [data-scheme='pride'], imported
// after index.css so its overrides win source-order ties (experiment/theme).
import './styles/pride-flourishes.css';

// Install the ring-buffer log capture as early as possible so bug reports
// (#158) include the maximal recent-log context. The buffer shadow-wraps
// console.{log,warn,error}; originals still fire, so DevTools is unaffected.
installLogBuffer();

// Apply persisted theme before React renders to prevent flash
try {
  const stored = localStorage.getItem('concord-settings');
  if (stored) {
    const { state } = JSON.parse(stored);
    const theme = state?.appearance?.theme || 'dark';
    let resolved: string;
    if (theme === 'system') {
      resolved = globalThis.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
    } else {
      resolved = theme;
    }
    document.documentElement.dataset.theme = resolved;

    // Apply compact mode
    if (state?.appearance?.compactMode) {
      document.documentElement.dataset.compact = 'true';
    }

    // Apply color scheme
    const colorScheme = state?.appearance?.colorScheme;
    if (colorScheme === 'custom') {
      // Apply cached derived custom theme variables synchronously
      const cachedVars = localStorage.getItem('concord-custom-theme-vars');
      if (cachedVars) {
        try {
          const vars = JSON.parse(cachedVars);
          const el = document.documentElement;
          for (const [prop, value] of Object.entries(vars)) {
            el.style.setProperty(prop, value as string);
          }
        } catch {
          // Ignore parse errors, store subscriber will re-apply
        }
      }
    } else if (colorScheme && colorScheme !== 'concord') {
      document.documentElement.dataset.scheme = colorScheme;
    }

    // Apply font size
    const fontSize = state?.appearance?.fontSize;
    if (fontSize && fontSize !== 'default') {
      document.documentElement.dataset.fontsize = fontSize;
    }
  }
} catch {
  // Ignore parse errors, defaults will be used
}

// Migration: remove legacy plaintext auth tokens from localStorage.
// Auth tokens are now encrypted via safeStorage in the main process.
localStorage.removeItem('concord-auth');

// Install SPA self-heal listeners as early as possible — before createRoot()
// or any lazy import() can fire (#753, ADR-0001).
installSelfHealHandlers();

const container = document.getElementById('root');

if (!container) {
  throw new Error('Root element not found');
}

const root = createRoot(container);

root.render(
  <React.StrictMode>
    <HashRouter>
      <ModalProvider>
        <App />
      </ModalProvider>
    </HashRouter>
  </React.StrictMode>
);
