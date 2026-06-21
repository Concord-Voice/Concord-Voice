import React from 'react';
import { render, screen, fireEvent } from '../../../test-utils';

vi.mock('@/renderer/components/Permissions/PermissionGrid.css', () => ({}));

import PermissionGrid from '@/renderer/components/Permissions/PermissionGrid';
import { PERMISSION_CATEGORIES } from '@/renderer/utils/permissions';

// Find the first permission bit from the categories for testing
const firstPerm = PERMISSION_CATEGORIES[0].permissions[0];
const firstBit = firstPerm.bit;

describe('PermissionGrid', () => {
  const mockOnChange = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
  });

  // --- Role mode ---

  describe('role mode', () => {
    it('renders all permission categories', () => {
      render(<PermissionGrid value={0n} onChange={mockOnChange} />);
      for (const cat of PERMISSION_CATEGORIES) {
        expect(screen.getByText(cat.name)).toBeInTheDocument();
      }
    });

    it('renders permission labels and descriptions', () => {
      render(<PermissionGrid value={0n} onChange={mockOnChange} />);
      expect(screen.getByText(firstPerm.label)).toBeInTheDocument();
      expect(screen.getByText(firstPerm.description)).toBeInTheDocument();
    });

    it('renders toggle switches with aria attributes', () => {
      render(<PermissionGrid value={0n} onChange={mockOnChange} />);
      const switches = screen.getAllByRole('switch');
      expect(switches.length).toBeGreaterThan(0);
      // First switch should be unchecked when bit is not set
      expect(switches[0]).toHaveAttribute('aria-checked', 'false');
    });

    it('shows active toggle when permission bit is set', () => {
      render(<PermissionGrid value={firstBit} onChange={mockOnChange} />);
      const switches = screen.getAllByRole('switch');
      expect(switches[0]).toHaveAttribute('aria-checked', 'true');
    });

    it('calls onChange to toggle a permission on', () => {
      render(<PermissionGrid value={0n} onChange={mockOnChange} />);
      const switches = screen.getAllByRole('switch');
      fireEvent.click(switches[0]);
      // Should set the bit (OR with the bit)
      expect(mockOnChange).toHaveBeenCalledWith(firstBit);
    });

    it('calls onChange to toggle a permission off', () => {
      render(<PermissionGrid value={firstBit} onChange={mockOnChange} />);
      const switches = screen.getAllByRole('switch');
      fireEvent.click(switches[0]);
      // Should clear the bit (AND NOT the bit)
      expect(mockOnChange).toHaveBeenCalledWith(0n);
    });

    it('does not call onChange when disabled', () => {
      render(<PermissionGrid value={0n} onChange={mockOnChange} disabled />);
      const switches = screen.getAllByRole('switch');
      fireEvent.click(switches[0]);
      expect(mockOnChange).not.toHaveBeenCalled();
    });

    it('toggles on Enter keypress', () => {
      render(<PermissionGrid value={0n} onChange={mockOnChange} />);
      const switches = screen.getAllByRole('switch');
      fireEvent.keyDown(switches[0], { key: 'Enter' });
      expect(mockOnChange).toHaveBeenCalled();
    });

    it('toggles on Space keypress', () => {
      render(<PermissionGrid value={0n} onChange={mockOnChange} />);
      const switches = screen.getAllByRole('switch');
      fireEvent.keyDown(switches[0], { key: ' ' });
      expect(mockOnChange).toHaveBeenCalled();
    });

    it('does not toggle on other keys', () => {
      render(<PermissionGrid value={0n} onChange={mockOnChange} />);
      const switches = screen.getAllByRole('switch');
      fireEvent.keyDown(switches[0], { key: 'Tab' });
      expect(mockOnChange).not.toHaveBeenCalled();
    });
  });

  // --- Override mode ---

  describe('override mode', () => {
    const mockOnDenyChange = vi.fn();

    beforeEach(() => {
      mockOnDenyChange.mockClear();
    });

    it('renders tristate buttons (Allow, Neutral, Deny)', () => {
      render(
        <PermissionGrid
          value={0n}
          onChange={mockOnChange}
          mode="override"
          deny={0n}
          onDenyChange={mockOnDenyChange}
        />
      );
      expect(screen.getAllByTitle('Allow').length).toBeGreaterThan(0);
      expect(screen.getAllByTitle('Neutral').length).toBeGreaterThan(0);
      expect(screen.getAllByTitle('Deny').length).toBeGreaterThan(0);
    });

    it('highlights Allow when bit is set in allow', () => {
      const { container } = render(
        <PermissionGrid
          value={firstBit}
          onChange={mockOnChange}
          mode="override"
          deny={0n}
          onDenyChange={mockOnDenyChange}
        />
      );
      const allowBtns = container.querySelectorAll('.permission-tristate-btn.allow-active');
      expect(allowBtns.length).toBeGreaterThan(0);
    });

    it('highlights Deny when bit is set in deny', () => {
      const { container } = render(
        <PermissionGrid
          value={0n}
          onChange={mockOnChange}
          mode="override"
          deny={firstBit}
          onDenyChange={mockOnDenyChange}
        />
      );
      const denyBtns = container.querySelectorAll('.permission-tristate-btn.deny-active');
      expect(denyBtns.length).toBeGreaterThan(0);
    });

    it('clicking Allow sets allow bit and clears deny', () => {
      render(
        <PermissionGrid
          value={0n}
          onChange={mockOnChange}
          mode="override"
          deny={firstBit}
          onDenyChange={mockOnDenyChange}
        />
      );
      // Click the first Allow button
      const allowBtns = screen.getAllByTitle('Allow');
      fireEvent.click(allowBtns[0]);
      expect(mockOnChange).toHaveBeenCalledWith(firstBit);
      expect(mockOnDenyChange).toHaveBeenCalledWith(0n);
    });

    it('clicking Deny sets deny bit and clears allow', () => {
      render(
        <PermissionGrid
          value={firstBit}
          onChange={mockOnChange}
          mode="override"
          deny={0n}
          onDenyChange={mockOnDenyChange}
        />
      );
      const denyBtns = screen.getAllByTitle('Deny');
      fireEvent.click(denyBtns[0]);
      expect(mockOnChange).toHaveBeenCalledWith(0n);
      expect(mockOnDenyChange).toHaveBeenCalledWith(firstBit);
    });

    it('clicking Neutral clears both allow and deny', () => {
      render(
        <PermissionGrid
          value={firstBit}
          onChange={mockOnChange}
          mode="override"
          deny={0n}
          onDenyChange={mockOnDenyChange}
        />
      );
      const neutralBtns = screen.getAllByTitle('Neutral');
      fireEvent.click(neutralBtns[0]);
      expect(mockOnChange).toHaveBeenCalledWith(0n);
      expect(mockOnDenyChange).toHaveBeenCalledWith(0n);
    });

    it('clicking Allow when already allowed returns to neutral', () => {
      render(
        <PermissionGrid
          value={firstBit}
          onChange={mockOnChange}
          mode="override"
          deny={0n}
          onDenyChange={mockOnDenyChange}
        />
      );
      const allowBtns = screen.getAllByTitle('Allow');
      fireEvent.click(allowBtns[0]);
      // Should clear the bit (toggle to neutral)
      expect(mockOnChange).toHaveBeenCalledWith(0n);
    });

    it('clicking Deny when already denied returns to neutral', () => {
      render(
        <PermissionGrid
          value={0n}
          onChange={mockOnChange}
          mode="override"
          deny={firstBit}
          onDenyChange={mockOnDenyChange}
        />
      );
      const denyBtns = screen.getAllByTitle('Deny');
      fireEvent.click(denyBtns[0]);
      expect(mockOnChange).toHaveBeenCalledWith(0n);
      expect(mockOnDenyChange).toHaveBeenCalledWith(0n);
    });

    it('does not call handlers when disabled', () => {
      render(
        <PermissionGrid
          value={0n}
          onChange={mockOnChange}
          mode="override"
          deny={0n}
          onDenyChange={mockOnDenyChange}
          disabled
        />
      );
      const allowBtns = screen.getAllByTitle('Allow');
      fireEvent.click(allowBtns[0]);
      expect(mockOnChange).not.toHaveBeenCalled();
      expect(mockOnDenyChange).not.toHaveBeenCalled();
    });
  });

  describe('disabled styling', () => {
    it('adds disabled class to permission rows', () => {
      const { container } = render(<PermissionGrid value={0n} onChange={mockOnChange} disabled />);
      const disabledRows = container.querySelectorAll('.permission-row.disabled');
      expect(disabledRows.length).toBeGreaterThan(0);
    });
  });
});
