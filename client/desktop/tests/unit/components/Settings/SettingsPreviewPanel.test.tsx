import { render, screen } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import SettingsPreviewPanel from '@/renderer/components/Settings/SettingsPreviewPanel';

// SettingsPreviewPanel is purely presentational — no store reads, no fetch,
// no WS. We assert that the structural markers a sighted user would scan are
// present and that the components use the real `.message-*` /
// `.participant-tile-*` class names so they inherit the doc-root cascade.

describe('SettingsPreviewPanel (#489)', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('renders the Live Preview collapsible section', () => {
    render(<SettingsPreviewPanel />);
    expect(screen.getByText('Live Preview')).toBeInTheDocument();
  });

  it('renders both Text Chat and Voice Chat preview labels', () => {
    render(<SettingsPreviewPanel />);
    expect(screen.getByText('Text Chat')).toBeInTheDocument();
    expect(screen.getByText('Voice Chat')).toBeInTheDocument();
  });

  describe('chat preview', () => {
    it('shows two distinct sample authors', () => {
      const { container } = render(<SettingsPreviewPanel />);
      // alice + bob each appear in both the chat preview AND the voice
      // preview (same sample names — intentional, so the user can mentally
      // tie the chat author to the voice tile). Assert via the chat-scoped
      // .message-username class to disambiguate.
      const chatUsernames = Array.from(container.querySelectorAll('.message-username')).map(
        (el) => el.textContent
      );
      expect(chatUsernames).toEqual(['alice', 'bob']);
    });

    it('includes a reply preview (Discord-style quoted parent)', () => {
      render(<SettingsPreviewPanel />);
      expect(screen.getByText('@alice')).toBeInTheDocument();
      expect(screen.getByText(/shipped the migration last night/)).toBeInTheDocument();
    });

    it('includes a reaction with a count', () => {
      const { container } = render(<SettingsPreviewPanel />);
      // Assert the structural reaction-row marker is present + count = 2
      const reaction = container.querySelector('.settings-preview-reaction');
      expect(reaction).toBeInTheDocument();
      expect(reaction?.textContent).toMatch(/2/);
    });

    it('uses real .message-username class so theme cascade applies', () => {
      const { container } = render(<SettingsPreviewPanel />);
      const usernames = container.querySelectorAll('.message-username');
      // Two distinct authors with .message-username applied (third message
      // intentionally omits the header — same-author rapid-reply pattern).
      expect(usernames.length).toBe(2);
    });
  });

  describe('voice preview', () => {
    it('renders 4 participant tiles', () => {
      const { container } = render(<SettingsPreviewPanel />);
      expect(container.querySelectorAll('.participant-tile').length).toBe(4);
    });

    it('marks one tile as speaking (accent outline)', () => {
      const { container } = render(<SettingsPreviewPanel />);
      expect(container.querySelectorAll('.settings-preview-tile-speaking').length).toBe(1);
    });

    it('one tile renders a video stub (gradient background)', () => {
      const { container } = render(<SettingsPreviewPanel />);
      expect(container.querySelectorAll('.participant-tile--video').length).toBe(1);
      expect(container.querySelectorAll('.settings-preview-video-stub').length).toBe(1);
    });

    it('three tiles render avatar-only with fallback initial', () => {
      const { container } = render(<SettingsPreviewPanel />);
      // Three avatar-only fallbacks (B / C / D); video tile #1 has no avatar.
      expect(container.querySelectorAll('.participant-tile__avatar-fallback').length).toBe(3);
    });

    it('voice state icons are aria-labelled (muted, deafened)', () => {
      render(<SettingsPreviewPanel />);
      expect(screen.getByLabelText('Muted')).toBeInTheDocument();
      expect(screen.getByLabelText('Deafened')).toBeInTheDocument();
    });
  });
});
