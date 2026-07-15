import { describe, it, expect, afterEach } from 'vitest';
import { render, cleanup, screen } from '@testing-library/react';
import ServerHoverTooltip from '@/renderer/components/Layout/ServerHoverTooltip';
import { mockServer } from '../../../mocks/fixtures';

function makeRect(overrides: Partial<DOMRect> = {}): DOMRect {
  return {
    top: 10,
    left: 20,
    right: 30,
    bottom: 40,
    width: 50,
    height: 60,
    x: 20,
    y: 10,
    toJSON: () => ({}),
    ...overrides,
  } as DOMRect;
}

describe('ServerHoverTooltip', () => {
  afterEach(() => {
    cleanup();
  });

  it('renders the server name, member/online counts, and voice count', () => {
    render(
      <ServerHoverTooltip
        server={{ ...mockServer, member_count: 5, online_count: 3 }}
        rect={makeRect()}
        voiceCount={2}
        showUnread={false}
        placement="below"
      />
    );

    expect(screen.getByText(mockServer.name)).toBeInTheDocument();
    expect(screen.getByText('5 Members')).toBeInTheDocument();
    expect(screen.getByText('3 Online')).toBeInTheDocument();
    expect(screen.getByText('2 In Voice')).toBeInTheDocument();
  });

  it('falls back to zero when member/online counts are undefined', () => {
    render(
      <ServerHoverTooltip
        server={{ ...mockServer, member_count: undefined, online_count: undefined }}
        rect={makeRect()}
        voiceCount={0}
        showUnread={false}
        placement="below"
      />
    );

    expect(screen.getByText('0 Members')).toBeInTheDocument();
    expect(screen.getByText('0 Online')).toBeInTheDocument();
  });

  it('applies the voice-active class only when voiceCount is greater than zero', () => {
    const { rerender } = render(
      <ServerHoverTooltip
        server={mockServer}
        rect={makeRect()}
        voiceCount={0}
        showUnread={false}
        placement="below"
      />
    );

    expect(screen.getByText('0 In Voice').className).toBe('server-bar-tooltip-voice');

    rerender(
      <ServerHoverTooltip
        server={mockServer}
        rect={makeRect()}
        voiceCount={1}
        showUnread={false}
        placement="below"
      />
    );

    expect(screen.getByText('1 In Voice').className).toBe(
      'server-bar-tooltip-voice server-bar-tooltip-voice--active'
    );
  });

  it('renders "Unread notifications" only when showUnread is true', () => {
    const { rerender } = render(
      <ServerHoverTooltip
        server={mockServer}
        rect={makeRect()}
        voiceCount={0}
        showUnread={false}
        placement="below"
      />
    );

    expect(screen.queryByText('Unread notifications')).not.toBeInTheDocument();

    rerender(
      <ServerHoverTooltip
        server={mockServer}
        rect={makeRect()}
        voiceCount={0}
        showUnread={true}
        placement="below"
      />
    );

    expect(screen.getByText('Unread notifications')).toBeInTheDocument();
  });

  it('positions below the rect when placement is "below"', () => {
    const rect = makeRect({ top: 10, left: 20, right: 30, bottom: 40, width: 50, height: 60 });
    render(
      <ServerHoverTooltip
        server={mockServer}
        rect={rect}
        voiceCount={0}
        showUnread={false}
        placement="below"
      />
    );

    const tooltip = screen.getByText(mockServer.name).closest('.server-bar-tooltip-fixed');
    expect(tooltip).not.toBeNull();
    const style = (tooltip as HTMLElement).style;
    expect(style.top).toBe('48px');
    expect(style.left).toBe('45px');
    expect(style.transform).toBe('translateX(-50%)');
  });

  it('positions to the right of the rect when placement is "right"', () => {
    const rect = makeRect({ top: 10, left: 20, right: 30, bottom: 40, width: 50, height: 60 });
    render(
      <ServerHoverTooltip
        server={mockServer}
        rect={rect}
        voiceCount={0}
        showUnread={false}
        placement="right"
      />
    );

    const tooltip = screen.getByText(mockServer.name).closest('.server-bar-tooltip-fixed');
    expect(tooltip).not.toBeNull();
    const style = (tooltip as HTMLElement).style;
    expect(style.top).toBe('40px');
    expect(style.left).toBe('38px');
    expect(style.transform).toBe('translateY(-50%)');
  });

  it('renders into a portal on document.body', () => {
    render(
      <ServerHoverTooltip
        server={mockServer}
        rect={makeRect()}
        voiceCount={0}
        showUnread={false}
        placement="below"
      />
    );

    const tooltip = document.body.querySelector(':scope > .server-bar-tooltip-fixed');
    expect(tooltip).not.toBeNull();
  });
});
