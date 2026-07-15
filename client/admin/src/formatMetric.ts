import type { MetricUnit } from "./contracts";

const numberFormatter = new Intl.NumberFormat("en-US", {
  maximumFractionDigits: 2,
});

function scaled(value: number, divisor: number, suffix: string): string {
  return `${numberFormatter.format(value / divisor)} ${suffix}`;
}

export function formatScalar(value: number, unit: MetricUnit): string {
  switch (unit) {
    case "percent":
      return `${numberFormatter.format(value)}%`;
    case "bytes":
      if (Math.abs(value) >= 1024 ** 3) return scaled(value, 1024 ** 3, "GB");
      if (Math.abs(value) >= 1024 ** 2) return scaled(value, 1024 ** 2, "MB");
      if (Math.abs(value) >= 1024) return scaled(value, 1024, "KB");
      return `${numberFormatter.format(value)} B`;
    case "bits_per_second":
      if (Math.abs(value) >= 1_000_000_000)
        return scaled(value, 1_000_000_000, "Gb/s");
      if (Math.abs(value) >= 1_000_000) return scaled(value, 1_000_000, "Mb/s");
      if (Math.abs(value) >= 1_000) return scaled(value, 1_000, "Kb/s");
      return `${numberFormatter.format(value)} b/s`;
    case "hours":
      return `${numberFormatter.format(value)} h`;
    case "count":
    case "load":
      return numberFormatter.format(value);
  }
}
