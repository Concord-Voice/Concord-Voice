import { describe, expect, it } from "vitest";

import { formatScalar } from "./formatMetric";

describe("formatScalar", () => {
  it.each([
    [1023, "1,023 B"],
    [1024, "1 KB"],
    [1025, "1 KB"],
    [1024 ** 2 - 1, "1,024 KB"],
    [1024 ** 2, "1 MB"],
    [1024 ** 2 + 1, "1 MB"],
    [1024 ** 3 - 1, "1,024 MB"],
    [1024 ** 3, "1 GB"],
    [1024 ** 3 + 1, "1 GB"],
    [-1024, "-1 KB"],
  ] as const)("formats %s bytes as %s", (value, expected) => {
    expect(formatScalar(value, "bytes")).toBe(expected);
  });

  it.each([
    [999, "999 b/s"],
    [1000, "1 Kb/s"],
    [1001, "1 Kb/s"],
    [1_000_000 - 1, "1,000 Kb/s"],
    [1_000_000, "1 Mb/s"],
    [1_000_000 + 1, "1 Mb/s"],
    [1_000_000_000 - 1, "1,000 Mb/s"],
    [1_000_000_000, "1 Gb/s"],
    [1_000_000_000 + 1, "1 Gb/s"],
    [-1_000_000, "-1 Mb/s"],
  ] as const)("formats %s bits per second as %s", (value, expected) => {
    expect(formatScalar(value, "bits_per_second")).toBe(expected);
  });

  it.each([
    ["percent", 12.345, "12.35%"],
    ["hours", 12.345, "12.35 h"],
    ["count", 1234.567, "1,234.57"],
    ["load", 1.234, "1.23"],
  ] as const)("formats %s values", (unit, value, expected) => {
    expect(formatScalar(value, unit)).toBe(expected);
  });
});
