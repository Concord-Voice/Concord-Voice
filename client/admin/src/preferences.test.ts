import { beforeEach, describe, expect, it } from "vitest";

import {
  DEFAULT_THRESHOLDS,
  FONT_STORAGE_KEY,
  THRESHOLD_STORAGE_KEY,
  loadFont,
  loadThresholds,
  resetThresholds,
  saveFont,
  saveThresholds,
  statusFor,
  validateThresholds,
} from "./preferences";

describe("threshold preferences", () => {
  beforeEach(() => {
    window.localStorage.clear();
  });

  it("uses the six approved defaults", () => {
    expect(DEFAULT_THRESHOLDS).toEqual({
      hostCpu: { warning: 75, critical: 90 },
      hostMemory: { warning: 80, critical: 90 },
      hostDisk: { warning: 80, critical: 90 },
      serviceCpu: { warning: 75, critical: 90 },
      http4xxShare: { warning: 5, critical: 10 },
      http5xxShare: { warning: 1, critical: 5 },
    });
  });

  it.each([
    [-1, 10],
    [10, 10],
    [11, 10],
    [10, 101],
    [Number.NaN, 90],
  ])("rejects invalid pair %s/%s", (warning, critical) => {
    const candidate = structuredClone(DEFAULT_THRESHOLDS);
    candidate.hostCpu = { warning, critical };
    expect(validateThresholds(candidate)).toBe(false);
  });

  it("allows service CPU thresholds above one core without relaxing percentage bounds", () => {
    const candidate = structuredClone(DEFAULT_THRESHOLDS);
    candidate.serviceCpu = { warning: 150, critical: 250 };

    expect(validateThresholds(candidate)).toBe(true);

    candidate.hostCpu = { warning: 150, critical: 250 };
    expect(validateThresholds(candidate)).toBe(false);
  });

  it("persists only a fully validated fixed threshold object", () => {
    const candidate = structuredClone(DEFAULT_THRESHOLDS);
    candidate.hostCpu = { warning: 70, critical: 95 };

    expect(saveThresholds(candidate)).toBe(true);
    expect(loadThresholds()).toEqual(candidate);
    expect(Object.keys(window.localStorage)).toEqual([THRESHOLD_STORAGE_KEY]);

    expect(
      saveThresholds({ ...candidate, arbitrary: { warning: 1, critical: 2 } }),
    ).toBe(false);
    expect(loadThresholds()).toEqual(candidate);
  });

  it("falls back on corrupt data and removes saved data on reset", () => {
    window.localStorage.setItem(THRESHOLD_STORAGE_KEY, "{broken");
    expect(loadThresholds()).toEqual(DEFAULT_THRESHOLDS);

    saveThresholds(DEFAULT_THRESHOLDS);
    resetThresholds();
    expect(window.localStorage.getItem(THRESHOLD_STORAGE_KEY)).toBeNull();
    expect(loadThresholds()).toEqual(DEFAULT_THRESHOLDS);
  });

  it("enters warning and critical state at the configured edges", () => {
    const pair = { warning: 75, critical: 90 };
    expect(statusFor(74.9, pair)).toBe("normal");
    expect(statusFor(75, pair)).toBe("warning");
    expect(statusFor(90, pair)).toBe("critical");
  });
});

describe("font preference", () => {
  it("allows only the three fixed font identifiers", () => {
    expect(loadFont()).toBe("source-sans");
    expect(saveFont("atkinson")).toBe(true);
    expect(loadFont()).toBe("atkinson");
    expect(window.localStorage.getItem(FONT_STORAGE_KEY)).toBe("atkinson");
    expect(saveFont("open-dyslexic")).toBe(true);
    expect(saveFont("remote-font")).toBe(false);
    expect(loadFont()).toBe("open-dyslexic");
  });
});
