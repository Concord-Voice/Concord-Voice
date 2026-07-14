export const THRESHOLD_STORAGE_KEY = "cv.admin.thresholds.v1";
export const FONT_STORAGE_KEY = "cv.admin.font.v1";

export type ThresholdName =
  | "hostCpu"
  | "hostMemory"
  | "hostDisk"
  | "serviceCpu"
  | "http4xxShare"
  | "http5xxShare";

export interface ThresholdPair {
  warning: number;
  critical: number;
}

export type Thresholds = Record<ThresholdName, ThresholdPair>;
export type ThresholdStatus = "normal" | "warning" | "critical";
export type FontChoice = "source-sans" | "atkinson" | "open-dyslexic";

export const THRESHOLD_MAXIMUMS: Readonly<Record<ThresholdName, number>> = {
  hostCpu: 100,
  hostMemory: 100,
  hostDisk: 100,
  serviceCpu: 1_000_000,
  http4xxShare: 100,
  http5xxShare: 100,
};

const thresholdNames: ThresholdName[] = [
  "hostCpu",
  "hostMemory",
  "hostDisk",
  "serviceCpu",
  "http4xxShare",
  "http5xxShare",
];

const fontChoices = new Set<string>([
  "source-sans",
  "atkinson",
  "open-dyslexic",
]);

export const DEFAULT_THRESHOLDS: Thresholds = {
  hostCpu: { warning: 75, critical: 90 },
  hostMemory: { warning: 80, critical: 90 },
  hostDisk: { warning: 80, critical: 90 },
  serviceCpu: { warning: 75, critical: 90 },
  http4xxShare: { warning: 5, critical: 10 },
  http5xxShare: { warning: 1, critical: 5 },
};

function copyThresholds(value: Thresholds): Thresholds {
  return Object.fromEntries(
    thresholdNames.map((name) => [
      name,
      { warning: value[name].warning, critical: value[name].critical },
    ]),
  ) as Thresholds;
}

function isPair(value: unknown, maximum: number): value is ThresholdPair {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const pair = value as Record<string, unknown>;
  if (
    Object.keys(pair).length !== 2 ||
    !("warning" in pair) ||
    !("critical" in pair)
  )
    return false;
  const { warning, critical } = pair;
  return (
    typeof warning === "number" &&
    Number.isFinite(warning) &&
    typeof critical === "number" &&
    Number.isFinite(critical) &&
    warning >= 0 &&
    warning < critical &&
    critical <= maximum
  );
}

export function validateThresholds(value: unknown): value is Thresholds {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const candidate = value as Record<string, unknown>;
  const actualNames = Object.keys(candidate);
  return (
    actualNames.length === thresholdNames.length &&
    thresholdNames.every(
      (name) =>
        Object.hasOwn(candidate, name) &&
        isPair(candidate[name], THRESHOLD_MAXIMUMS[name]),
    )
  );
}

export function loadThresholds(): Thresholds {
  try {
    const raw = globalThis.localStorage.getItem(THRESHOLD_STORAGE_KEY);
    if (raw) {
      const parsed: unknown = JSON.parse(raw);
      if (validateThresholds(parsed)) return copyThresholds(parsed);
    }
  } catch {
    // Storage can be unavailable under hardened browser policies.
  }
  return copyThresholds(DEFAULT_THRESHOLDS);
}

export function saveThresholds(value: unknown): boolean {
  if (!validateThresholds(value)) return false;
  try {
    globalThis.localStorage.setItem(
      THRESHOLD_STORAGE_KEY,
      JSON.stringify(copyThresholds(value)),
    );
    return true;
  } catch {
    return false;
  }
}

export function resetThresholds(): void {
  try {
    globalThis.localStorage.removeItem(THRESHOLD_STORAGE_KEY);
  } catch {
    // Reset is best-effort when browser storage is disabled.
  }
}

export function statusFor(value: number, pair: ThresholdPair): ThresholdStatus {
  if (value >= pair.critical) return "critical";
  if (value >= pair.warning) return "warning";
  return "normal";
}

export function loadFont(): FontChoice {
  try {
    const value = globalThis.localStorage.getItem(FONT_STORAGE_KEY);
    if (value && fontChoices.has(value)) return value as FontChoice;
  } catch {
    // Storage can be unavailable under hardened browser policies.
  }
  return "source-sans";
}

export function saveFont(value: string): value is FontChoice {
  if (!fontChoices.has(value)) return false;
  try {
    globalThis.localStorage.setItem(FONT_STORAGE_KEY, value);
    return true;
  } catch {
    return false;
  }
}
