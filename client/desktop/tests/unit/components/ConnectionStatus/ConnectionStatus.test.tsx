import { render, screen } from '../../../test-utils';
import ConnectionStatus from '@/renderer/components/ConnectionStatus/ConnectionStatus';
import { useChatStore } from '@/renderer/stores/chatStore';
import { resetAllStores } from '../../../helpers/store-helpers';

describe('ConnectionStatus', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('shows connected state', () => {
    useChatStore.setState({ connectionState: 'connected', connectionClientId: 'client-123' });
    render(<ConnectionStatus />);
    expect(screen.getByText('Connected')).toBeInTheDocument();
    const el = document.querySelector('.connection-status.connected');
    expect(el).toBeInTheDocument();
  });

  it('shows connecting state', () => {
    useChatStore.setState({ connectionState: 'connecting' });
    render(<ConnectionStatus />);
    expect(screen.getByText('Connecting')).toBeInTheDocument();
    const el = document.querySelector('.connection-status.connecting');
    expect(el).toBeInTheDocument();
  });

  it('shows disconnected state', () => {
    useChatStore.setState({ connectionState: 'disconnected' });
    render(<ConnectionStatus />);
    expect(screen.getByText('Offline')).toBeInTheDocument();
    const el = document.querySelector('.connection-status.disconnected');
    expect(el).toBeInTheDocument();
  });

  it('includes client ID in title when connected', () => {
    useChatStore.setState({ connectionState: 'connected', connectionClientId: 'abc-123' });
    render(<ConnectionStatus />);
    const el = document.querySelector('.connection-status');
    expect(el?.getAttribute('title')).toContain('abc-123');
  });
});
