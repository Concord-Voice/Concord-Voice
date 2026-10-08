import { vi } from 'vitest';
import { clientConfigService } from '@/renderer/services/system/clientConfigService';
import { useClientConfigStore } from '@/renderer/stores/ui/clientConfigStore';

// A purge dialog offers its pin choice only once a capability answer arrives
// after it opens (#3552). This stub answers each refresh with a new copy of
// the stored capabilities, as a server that has not changed would.
export function answerCapabilityRefresh() {
  return vi.spyOn(clientConfigService, 'refreshServerCapabilities').mockImplementation(async () => {
    const caps = useClientConfigStore.getState().serverCapabilities;
    if (caps) useClientConfigStore.setState({ serverCapabilities: { ...caps } });
  });
}
