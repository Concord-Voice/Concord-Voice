import { vi } from 'vitest';
import { render, screen, userEvent, within } from '../../../test-utils';
import CustomSelect from '@/renderer/components/ui/CustomSelect';

describe('CustomSelect', () => {
  it('forwards its id so a visible label provides the accessible name', () => {
    render(
      <>
        <label htmlFor="video-codec">Video Codec</label>
        <CustomSelect
          id="video-codec"
          options={[{ value: 'video/AV1', label: 'AV1' }]}
          value="video/AV1"
          onChange={() => {}}
        />
      </>
    );

    expect(screen.getByRole('combobox', { name: 'Video Codec' })).toHaveAttribute(
      'id',
      'video-codec'
    );
  });

  it('renders disabled options as unavailable to native selection', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(
      <CustomSelect
        options={[
          { value: 'video/AV1', label: 'AV1' },
          { value: 'video/VP9:2', label: 'VP9 (Profile 2 — HDR)', disabled: true },
        ]}
        value="video/AV1"
        onChange={onChange}
      />
    );

    const select = screen.getByRole('combobox');
    const hdrOption = within(select).getByRole('option', { name: 'VP9 (Profile 2 — HDR)' });
    expect(hdrOption).toBeDisabled();

    await user.selectOptions(select, hdrOption);
    expect(select).toHaveValue('video/AV1');
    expect(onChange).not.toHaveBeenCalled();
  });
});
