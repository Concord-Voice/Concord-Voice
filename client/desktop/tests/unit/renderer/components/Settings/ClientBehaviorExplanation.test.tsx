import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';
import { ClientBehaviorExplanation } from '@/renderer/components/Settings/ClientBehaviorExplanation';
import type { ClientBehavior } from '@/shared/clientBehavior';

describe('ClientBehaviorExplanation', () => {
  it('renders default config (close→tray, minimize→toolbar) correctly', () => {
    const cb: ClientBehavior = { toTray: 'close', toToolbar: 'minimize' };
    render(<ClientBehaviorExplanation clientBehavior={cb} />);
    expect(screen.getByText(/To Close Concord Voice/i)).toBeInTheDocument();
    expect(screen.getByText(/Quit button in the user menu/i)).toBeInTheDocument();
    expect(screen.getByText(/click the \[—\] button/i)).toBeInTheDocument();
    expect(screen.getByText(/click the \[×\] button/i)).toBeInTheDocument();
  });

  it('renders quit-config (none, minimize) correctly — [X] is a real quit button', () => {
    const cb: ClientBehavior = { toTray: 'none', toToolbar: 'minimize' };
    render(<ClientBehaviorExplanation clientBehavior={cb} />);
    expect(screen.getByText(/click the \[×\] button/i)).toBeInTheDocument();
    expect(screen.getByText(/Concord Voice will quit gracefully/i)).toBeInTheDocument();
    expect(screen.getByText(/No button is configured/i)).toBeInTheDocument();
  });

  it('renders swap-config (minimize, close) correctly', () => {
    const cb: ClientBehavior = { toTray: 'minimize', toToolbar: 'close' };
    render(<ClientBehaviorExplanation clientBehavior={cb} />);
    expect(screen.getByText(/Quit button in the user menu/i)).toBeInTheDocument();
  });
});
