export type TimeMode = "utc" | "local";

const options: Intl.DateTimeFormatOptions = {
  day: "numeric",
  hour: "2-digit",
  minute: "2-digit",
  month: "short",
  second: "2-digit",
  timeZoneName: "short",
  year: "numeric",
};

const formatters: Record<TimeMode, Intl.DateTimeFormat> = {
  utc: new Intl.DateTimeFormat("en-US", { ...options, timeZone: "UTC" }),
  local: new Intl.DateTimeFormat("en-US", options),
};

export function formatTimestamp(
  value: string | number,
  mode: TimeMode,
): string {
  return formatters[mode].format(new Date(value));
}
