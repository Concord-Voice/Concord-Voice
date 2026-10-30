import { describe, it, expect, vi } from 'vitest';

const mockSendSetStatus = vi.fn();
vi.mock('@/renderer/services/messaging/websocketService', () => ({
  getWebSocketService: () => ({ sendSetStatus: mockSendSetStatus }),
}));

import { setOwnPresence } from '@/renderer/services/presence/setOwnPresence';

describe('setOwnPresence', () => {
  it.each(['online', 'dnd', 'invisible'] as const)('sends %s over the websocket', (status) => {
    mockSendSetStatus.mockClear();
    setOwnPresence(status);
    expect(mockSendSetStatus).toHaveBeenCalledTimes(1);
    expect(mockSendSetStatus).toHaveBeenCalledWith(status);
  });
});
