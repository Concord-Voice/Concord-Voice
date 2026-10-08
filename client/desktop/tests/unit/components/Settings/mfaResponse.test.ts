import { describe, expect, it } from 'vitest';
import { refusalText } from '@/renderer/components/Settings/mfaResponse';

// The refused answers of the MFA enrolment requests that are not step-up-gated.
// Mutant: an unguarded `res.json()`, which surfaces a gateway page's parse
// error ("Unexpected token '<'") as the wizard's error.

const refused = (body: string, contentType = 'application/json') =>
  new Response(body, { status: 502, headers: { 'Content-Type': contentType } });

describe('refusalText', () => {
  it("returns the server's error text", async () => {
    expect(await refusalText(refused('{"error":"Invalid code"}'), 'Fallback')).toBe('Invalid code');
  });

  it.each([
    ['an HTML error page', '<html><body>Bad gateway</body></html>', 'text/html'],
    ['an empty body', '', 'text/plain'],
    ['JSON null', 'null', 'application/json'],
    ['a JSON string', '"nope"', 'application/json'],
    ['an object with no error', '{}', 'application/json'],
    ['an empty error', '{"error":""}', 'application/json'],
    ['an error that is not a string', '{"error":42}', 'application/json'],
  ])('falls back for %s', async (_name, body, contentType) => {
    expect(await refusalText(refused(body, contentType), 'Fallback')).toBe('Fallback');
  });
});
