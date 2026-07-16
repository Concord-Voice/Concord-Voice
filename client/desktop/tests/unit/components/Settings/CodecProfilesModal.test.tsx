import { useState } from 'react';
import { render, screen, userEvent, within } from '../../../test-utils';
import CodecProfilesModal from '@/renderer/components/Settings/CodecProfilesModal';
import { CODEC_PROFILE_GUIDE } from '@/renderer/components/Settings/codecMetadata';

describe('CodecProfilesModal', () => {
  it('renders the accessible profile guide and technical identifiers', () => {
    render(<CodecProfilesModal isOpen onClose={() => {}} />);

    const dialog = screen.getByRole('dialog', { name: 'What are codec profiles?' });
    expect(dialog).toHaveClass('modal-large');

    const table = screen.getByRole('table', { name: 'Codec profiles available in Concord' });
    const headers = within(table).getAllByRole('columnheader');
    expect(headers).toHaveLength(3);
    headers.forEach((header) => expect(header).toHaveAttribute('scope', 'col'));

    const rowHeaders = within(table).getAllByRole('rowheader');
    const renderedLabels = rowHeaders.map((header) => header.textContent);
    expect(renderedLabels).toEqual(CODEC_PROFILE_GUIDE.map((profile) => profile.label));
    expect(renderedLabels).toEqual([
      'AV1 (10-bit HDR target)',
      'AV1 (8-bit SDR target)',
      'AV1',
      'VP9 (Profile 2 — HDR)',
      'VP9 (Profile 0 — SVC)',
      'H.264 (High 5.2 — Best H.264 quality)',
      'H.264 (Main 5.0 — Balanced)',
      'H.264 (Constrained Baseline 3.1 — Compatibility)',
      'VP8',
    ]);
    rowHeaders.forEach((header) => expect(header).toHaveAttribute('scope', 'row'));

    CODEC_PROFILE_GUIDE.forEach((profile, index) => {
      const row = rowHeaders[index].closest('tr');
      expect(row).not.toBeNull();
      expect(within(row!).getByText(profile.standard)).toBeInTheDocument();
      if (profile.signal) {
        expect(within(row!).getByText(profile.signal).tagName).toBe('CODE');
      }
    });

    expect(within(table).getByText('profile=0').tagName).toBe('CODE');
    expect(within(table).getByText('profile-id=2').tagName).toBe('CODE');
    expect(within(table).getByText('profile-id=0').tagName).toBe('CODE');
    expect(within(table).getByText('profile-level-id=640034').tagName).toBe('CODE');
    expect(within(table).getByText('profile-level-id=4d0032').tagName).toBe('CODE');
    expect(within(table).getByText('profile-level-id=42e01f').tagName).toBe('CODE');

    expect(screen.getByText(/Supports 8- and 10-bit coding/i)).toBeInTheDocument();
    expect(screen.getByText(/does not guarantee a 10-bit HDR stream/i)).toBeInTheDocument();
    expect(screen.getByText(/High is an 8-bit SDR profile/i)).toBeInTheDocument();
    expect(screen.getByText(/High 10 is a different profile/i)).toBeInTheDocument();
    expect(screen.getByText(/system detection does not make it routable/i)).toBeInTheDocument();
  });

  it('closes on Escape and returns focus to the invoking button', async () => {
    const user = userEvent.setup();
    const root = document.createElement('div');
    root.id = 'root';
    document.body.appendChild(root);

    function Harness() {
      const [open, setOpen] = useState(false);
      return (
        <>
          <button type="button" onClick={() => setOpen(true)}>
            What are codec profiles?
          </button>
          <CodecProfilesModal isOpen={open} onClose={() => setOpen(false)} />
        </>
      );
    }

    render(<Harness />, { container: root });
    const trigger = screen.getByRole('button', { name: 'What are codec profiles?' });

    await user.click(trigger);
    expect(screen.getByRole('dialog', { name: 'What are codec profiles?' })).toHaveFocus();

    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog', { name: 'What are codec profiles?' })).toBeNull();
    expect(trigger).toHaveFocus();

    root.remove();
  });
});
