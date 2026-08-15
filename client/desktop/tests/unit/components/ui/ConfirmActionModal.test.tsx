import { render, screen, cleanup } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import ConfirmActionModal from '@/renderer/components/ui/ConfirmActionModal';

const noop = () => {};

const baseProps = {
  isOpen: true,
  onClose: noop,
  title: 'Ban Member',
  message: 'Body text',
  confirmLabel: 'Ban',
  loadingLabel: 'Banning...',
  onConfirm: async () => {},
};

// React's useId counter advances on every render, so two renders of the same
// tree legitimately produce different generated ids. Normalizing the
// id-carrying attributes keeps the structural comparison exact everywhere else.
const normalizeIds = (html: string) =>
  html.replace(/\b(id|for|aria-labelledby|aria-describedby)="[^"]*"/g, '$1="X"');

describe('ConfirmActionModal extraContent (#1354)', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('renders extraContent between the message and the confirmation input', () => {
    render(
      <ConfirmActionModal
        {...baseProps}
        extraContent={
          <label>
            <input type="checkbox" /> Extra
          </label>
        }
        confirmationInput={{ label: 'Type it', expectedValue: 'X' }}
      />
    );

    const html = screen.getByRole('dialog').innerHTML;
    expect(html.indexOf('Body text')).toBeLessThan(html.indexOf('Extra'));
    expect(html.indexOf('Extra')).toBeLessThan(html.indexOf('Type it'));
  });

  it('renders extraContent when there is no confirmation input', () => {
    render(
      <ConfirmActionModal
        {...baseProps}
        extraContent={
          <label>
            <input type="checkbox" /> Extra
          </label>
        }
      />
    );

    expect(screen.getByRole('checkbox', { name: 'Extra' })).toBeInTheDocument();
  });

  // The two comparison tests below only prove that an explicit `undefined`
  // behaves like an omitted prop. These two lock the shape against the
  // pre-#1354 markup, which is what "byte-identical for the eight existing
  // consumers" actually means — an always-rendered wrapper around
  // `extraContent` would survive the comparison tests but fail here.
  const childClasses = () =>
    Array.from(
      screen.getByRole('dialog').querySelector('.delete-server-content')?.children ?? []
    ).map((el) => el.className);

  it('adds no element to the tree when extraContent is undefined', () => {
    render(<ConfirmActionModal {...baseProps} />);
    expect(childClasses()).toEqual(['delete-server-warning', 'delete-server-actions']);
  });

  it('adds no element to the tree when extraContent is undefined and a confirmation input is present', () => {
    render(
      <ConfirmActionModal
        {...baseProps}
        confirmationInput={{ label: 'Type it', expectedValue: 'X' }}
      />
    );
    expect(childClasses()).toEqual([
      'delete-server-warning',
      'delete-server-confirm',
      'delete-server-actions',
    ]);
  });

  it('renders identically to before when extraContent is undefined', () => {
    render(<ConfirmActionModal {...baseProps} extraContent={undefined} />);
    const withProp = normalizeIds(screen.getByRole('dialog').innerHTML);
    cleanup();

    render(<ConfirmActionModal {...baseProps} />);
    const without = normalizeIds(screen.getByRole('dialog').innerHTML);

    expect(withProp).toBe(without);
  });

  it('renders identically to before when extraContent is undefined and a confirmation input is present', () => {
    const withInput = { ...baseProps, confirmationInput: { label: 'Type it', expectedValue: 'X' } };

    render(<ConfirmActionModal {...withInput} extraContent={undefined} />);
    const withProp = normalizeIds(screen.getByRole('dialog').innerHTML);
    cleanup();

    render(<ConfirmActionModal {...withInput} />);
    const without = normalizeIds(screen.getByRole('dialog').innerHTML);

    expect(withProp).toBe(without);
  });
});
