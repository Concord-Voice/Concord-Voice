import { render, screen, fireEvent } from '../../../test-utils';
import { vi } from 'vitest';
import ImageLightbox from '@/renderer/components/Chat/ImageLightbox';

describe('ImageLightbox', () => {
  const baseProps = { src: 'blob:fake-url', alt: 'pic', onClose: vi.fn() };

  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('renders the image and the dialog role (portaled to body)', () => {
    render(<ImageLightbox {...baseProps} />);
    expect(screen.getByAltText('pic')).toHaveAttribute('src', 'blob:fake-url');
    expect(screen.getByRole('dialog', { name: 'Image viewer' })).toBeInTheDocument();
  });

  it('focuses the close button on open (a11y)', () => {
    render(<ImageLightbox {...baseProps} />);
    expect(screen.getByLabelText('Close')).toHaveFocus();
  });

  it('closes on Escape', () => {
    const onClose = vi.fn();
    render(<ImageLightbox {...baseProps} onClose={onClose} />);
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('closes when the close button is clicked', () => {
    const onClose = vi.fn();
    render(<ImageLightbox {...baseProps} onClose={onClose} />);
    fireEvent.click(screen.getByLabelText('Close'));
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('closes on a backdrop click but NOT on an image click', () => {
    const onClose = vi.fn();
    render(<ImageLightbox {...baseProps} onClose={onClose} />);
    fireEvent.click(screen.getByAltText('pic'));
    expect(onClose).not.toHaveBeenCalled();
    fireEvent.click(screen.getByLabelText('Close image viewer')); // the backdrop <button>
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('zooms in / out / resets via the toolbar and reflects the percentage', () => {
    render(<ImageLightbox {...baseProps} />);
    expect(screen.getByText('100%')).toBeInTheDocument();
    // At minimum zoom, Zoom-out and Reset are disabled.
    expect(screen.getByLabelText('Zoom out')).toBeDisabled();
    expect(screen.getByLabelText('Reset zoom')).toBeDisabled();

    fireEvent.click(screen.getByLabelText('Zoom in'));
    expect(screen.getByText('150%')).toBeInTheDocument();
    expect(screen.getByLabelText('Zoom out')).toBeEnabled();

    // Zoom-out button steps back down (and re-centers at min).
    fireEvent.click(screen.getByLabelText('Zoom out'));
    expect(screen.getByText('100%')).toBeInTheDocument();

    fireEvent.click(screen.getByLabelText('Zoom in'));
    fireEvent.click(screen.getByLabelText('Reset zoom'));
    expect(screen.getByText('100%')).toBeInTheDocument();
    expect(screen.getByLabelText('Zoom out')).toBeDisabled();
  });

  it('shift+Tab focus trap wraps first → last', () => {
    render(<ImageLightbox {...baseProps} />);
    // At 1× the first enabled control is Zoom in; shift+Tab from it wraps to Close.
    screen.getByLabelText('Zoom in').focus();
    fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Tab', shiftKey: true });
    expect(screen.getByLabelText('Close')).toHaveFocus();
  });

  it('shows the Save control only when onSave is supplied, and invokes it', () => {
    const onSave = vi.fn();
    const { rerender } = render(<ImageLightbox {...baseProps} />);
    expect(screen.queryByLabelText('Save image')).not.toBeInTheDocument();

    rerender(<ImageLightbox {...baseProps} onSave={onSave} />);
    fireEvent.click(screen.getByLabelText('Save image'));
    expect(onSave).toHaveBeenCalledTimes(1);
  });

  it('zooms with the mouse wheel', () => {
    render(<ImageLightbox {...baseProps} />);
    fireEvent.wheel(screen.getByRole('dialog'), { deltaY: -100 });
    expect(screen.getByText('150%')).toBeInTheDocument();
    fireEvent.wheel(screen.getByRole('dialog'), { deltaY: 100 });
    expect(screen.getByText('100%')).toBeInTheDocument();
  });

  it('double-click toggles zoom to 200% and back', () => {
    render(<ImageLightbox {...baseProps} />);
    const img = screen.getByAltText('pic');
    fireEvent.doubleClick(img);
    expect(screen.getByText('200%')).toBeInTheDocument();
    fireEvent.doubleClick(img);
    expect(screen.getByText('100%')).toBeInTheDocument();
  });

  it('pans the image with a pointer drag once zoomed in', () => {
    render(<ImageLightbox {...baseProps} />);
    const img = screen.getByAltText('pic');
    // No pan at 1× — a drag leaves the transform un-offset.
    fireEvent.pointerDown(img, { clientX: 10, clientY: 10, pointerId: 1 });
    fireEvent.pointerMove(img, { clientX: 50, clientY: 50, pointerId: 1 });
    expect(img.style.transform).toContain('translate(0px, 0px)');
    fireEvent.pointerUp(img, { pointerId: 1 });

    // Zoom in, then drag → transform carries the offset.
    fireEvent.click(screen.getByLabelText('Zoom in'));
    fireEvent.pointerDown(img, { clientX: 100, clientY: 100, pointerId: 1 });
    fireEvent.pointerMove(img, { clientX: 130, clientY: 120, pointerId: 1 });
    expect(img.style.transform).toContain('translate(30px, 20px)');
    // After release, further movement does not pan.
    fireEvent.pointerUp(img, { pointerId: 1 });
    fireEvent.pointerMove(img, { clientX: 300, clientY: 300, pointerId: 1 });
    expect(img.style.transform).toContain('translate(30px, 20px)');
  });

  it('traps Tab focus within the overlay (wraps last → first enabled control)', () => {
    render(<ImageLightbox {...baseProps} />);
    screen.getByLabelText('Close').focus();
    fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Tab' });
    // Zoom-out/Reset are disabled at 1×, so the first enabled control is Zoom in.
    expect(screen.getByLabelText('Zoom in')).toHaveFocus();
  });
});
