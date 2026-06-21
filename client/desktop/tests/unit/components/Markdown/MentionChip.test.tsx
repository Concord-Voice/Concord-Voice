import { describe, it, expect } from 'vitest';
import { render, screen } from '../../../test-utils';
import MentionChip from '@/renderer/components/Markdown/MentionChip';

const lookup = {
  users: new Map([['abc-123', 'alice']]),
  roles: new Map([['role-42', 'Admin']]),
};

describe('MentionChip', () => {
  it('resolves <@userId> to @displayName', () => {
    render(<MentionChip token="<@abc-123>" lookup={lookup} />);
    expect(screen.getByText('@alice')).toBeInTheDocument();
  });

  it('resolves <@&roleId> to @roleName', () => {
    render(<MentionChip token="<@&role-42>" lookup={lookup} />);
    expect(screen.getByText('@Admin')).toBeInTheDocument();
  });

  it('falls back to token when userId is unknown', () => {
    render(<MentionChip token="<@unknown>" lookup={lookup} />);
    expect(screen.getByText('<@unknown>')).toBeInTheDocument();
  });

  it('renders plain @username unchanged', () => {
    render(<MentionChip token="@bob" lookup={lookup} />);
    expect(screen.getByText('@bob')).toBeInTheDocument();
  });
});
