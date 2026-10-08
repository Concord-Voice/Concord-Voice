import { useEffect, useState } from 'react';
import { clientConfigService } from '../../services/system/clientConfigService';
import { useClientConfigStore, usePurgeKeepsPinned } from '../../stores/ui/clientConfigStore';

/**
 * `usePurgeKeepsPinned` as it read when `isOpen` last turned true. Every purge
 * dialog samples the capability once per open (#3458, spec §9.1), so a
 * capability refresh cannot change what an open dialog offers or sends.
 * Adjusted during render rather than in an effect, so the first open paint
 * already has it.
 *
 * The sample is only offered once a capability answer that arrived AFTER the
 * open confirms it. Opening refreshes the capability, and until that answer
 * lands the dialog makes no pin claim and sends no `include_pinned`: the cached
 * value may predate a rollback, and a rolled-back server ignores the field and
 * deletes pins (#3552 review). A failed refresh resets the capabilities to
 * `null`, which confirms nothing, so the dialog stays without the claim.
 *
 * A server that answers without the capability withdraws the sample. The
 * withdrawal latches until close, because a failed fetch that follows it
 * resets the capabilities to `null`, which is not an answer. A capability that
 * appears while open is not offered until the next open.
 */
export function usePurgeKeepsPinnedAtOpen(isOpen: boolean): boolean {
  const keepsPinned = usePurgeKeepsPinned();
  const capabilities = useClientConfigStore((s) => s.serverCapabilities);
  const withdrawn = capabilities !== null && capabilities.features.purgeKeepsPinned !== true;
  const [sampledOpen, setSampledOpen] = useState(isOpen);
  const [sample, setSample] = useState(keepsPinned);
  // Each answer is a new object, so identity tells a fresh answer from the
  // cached one this open started with.
  const [answerAtOpen, setAnswerAtOpen] = useState(capabilities);
  const [confirmed, setConfirmed] = useState(false);
  const [latched, setLatched] = useState(false);
  if (isOpen !== sampledOpen) {
    setSampledOpen(isOpen);
    if (isOpen) {
      setSample(keepsPinned);
      setAnswerAtOpen(capabilities);
      setConfirmed(false);
      setLatched(false);
    }
  } else if (isOpen) {
    if (withdrawn && !latched) setLatched(true);
    if (!withdrawn && !confirmed && capabilities !== null && capabilities !== answerAtOpen) {
      setConfirmed(true);
    }
  }
  useEffect(() => {
    if (isOpen) void clientConfigService.refreshServerCapabilities();
  }, [isOpen]);
  return sample && confirmed && !withdrawn && !latched;
}
