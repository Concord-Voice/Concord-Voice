// Linear-time, non-backtracking regex — anchored, character-class only.
// Safe for SonarQube ReDoS detection and runtime.
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export function isValidUUID(value: string): boolean {
  return UUID_RE.test(value);
}

export function assertValidUUID(value: string, fieldName: string): string {
  if (!isValidUUID(value)) {
    throw new Error(`${fieldName} is not a valid UUID`);
  }
  return value;
}
