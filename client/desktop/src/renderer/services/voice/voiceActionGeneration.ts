// Shared by the voice service and connection recovery. The recovery hook must
// sample user voice actions before its lazy voice-service import can resolve.
let generation = 0;

export function getVoiceActionGeneration(): number {
  return generation;
}

export function advanceVoiceActionGeneration(): number {
  generation += 1;
  return generation;
}
