/** Match PostgreSQL's UTC timestamp + calendar-month interval at month ends. */
export function addCalendarMonthsUTC(isoDate: string, months: number): Date | null {
  const start = new Date(isoDate);
  if (Number.isNaN(start.getTime()) || !Number.isInteger(months) || months < 1) return null;
  const first = new Date(
    Date.UTC(
      start.getUTCFullYear(),
      start.getUTCMonth() + months,
      1,
      start.getUTCHours(),
      start.getUTCMinutes(),
      start.getUTCSeconds(),
      start.getUTCMilliseconds()
    )
  );
  if (Number.isNaN(first.getTime())) return null;
  const lastDay = new Date(
    Date.UTC(first.getUTCFullYear(), first.getUTCMonth() + 1, 0)
  ).getUTCDate();
  first.setUTCDate(Math.min(start.getUTCDate(), lastDay));
  return first;
}

/** Older servers still publish an elapsed-seconds cooldown. */
export function addElapsedSeconds(isoDate: string, seconds: number): Date | null {
  const start = new Date(isoDate);
  if (Number.isNaN(start.getTime()) || !Number.isInteger(seconds) || seconds < 1) return null;
  const end = new Date(start.getTime() + seconds * 1000);
  return Number.isNaN(end.getTime()) ? null : end;
}
