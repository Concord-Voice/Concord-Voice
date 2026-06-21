/**
 * Regression test for the resolveMediaUrl media-src rule.
 *
 * Locks the behaviour of the two `no-restricted-syntax` selectors added to
 * `eslint.config.mjs` for issue #1586. If this test breaks, the rule has been
 * weakened or removed — either fix the rule or confirm the weakening is
 * intentional (and update the test).
 *
 * Spec: [internal]specs/2026-06-17-1586-resolve-media-url-design.md
 */
import { ESLint } from 'eslint';
import path from 'node:path';
import { describe, expect, it } from 'vitest';

const CLIENT_DESKTOP_ROOT = path.resolve(__dirname, '../../..');

async function lintRenderer(
  code: string
): Promise<Array<{ ruleId: string | null; message: string }>> {
  const eslint = new ESLint({ cwd: CLIENT_DESKTOP_ROOT });
  // Virtual renderer fixture — must be in eslint.config.mjs's
  // projectService.allowDefaultProject allowlist (already present).
  const [result] = await eslint.lintText(code, {
    filePath: path.join(CLIENT_DESKTOP_ROOT, 'src/renderer/__lint-fixture__.tsx'),
  });
  return result.messages.map((m) => ({ ruleId: m.ruleId, message: m.message }));
}

function hasMediaSrcViolation(
  messages: Array<{ ruleId: string | null; message: string }>
): boolean {
  return messages.some(
    (m) => m.ruleId === 'no-restricted-syntax' && /resolveMediaUrl/.test(m.message)
  );
}

describe('no-restricted-syntax — server-origin media <img src> in renderer', () => {
  it('flags <img src={obj.avatar_url}> (member access)', async () => {
    const messages = await lintRenderer(`
      export const A = ({ user }: { user: { avatar_url: string } }) => (
        <img src={user.avatar_url} alt="" />
      );
    `);
    expect(hasMediaSrcViolation(messages)).toBe(true);
  });

  it('flags <img src={obj.header_image_url}> and <img src={obj.icon_url}>', async () => {
    const banner = await lintRenderer(`
      export const B = ({ p }: { p: { header_image_url: string } }) => <img src={p.header_image_url} alt="" />;
    `);
    const icon = await lintRenderer(`
      export const C = ({ s }: { s: { icon_url: string } }) => <img src={s.icon_url} alt="" />;
    `);
    expect(hasMediaSrcViolation(banner)).toBe(true);
    expect(hasMediaSrcViolation(icon)).toBe(true);
  });

  it('flags bare <img src={avatarUrl}> (destructured identifier)', async () => {
    const messages = await lintRenderer(`
      export const D = ({ avatarUrl }: { avatarUrl: string }) => <img src={avatarUrl} alt="" />;
    `);
    expect(hasMediaSrcViolation(messages)).toBe(true);
  });

  it('allows <img src={resolveMediaUrl(obj.avatar_url)}> (wrapped)', async () => {
    const messages = await lintRenderer(`
      const resolveMediaUrl = (s?: string) => s;
      export const E = ({ user }: { user: { avatar_url: string } }) => (
        <img src={resolveMediaUrl(user.avatar_url)} alt="" />
      );
    `);
    expect(hasMediaSrcViolation(messages)).toBe(false);
  });

  it('allows local preview/objectURL fields not in the media-field allowlist', async () => {
    const preview = await lintRenderer(`
      export const F = ({ avatar }: { avatar: { preview: string } }) => <img src={avatar.preview} alt="" />;
    `);
    const objectUrl = await lintRenderer(`
      export const G = ({ url }: { url: string }) => <img src={url} alt="" />;
    `);
    expect(hasMediaSrcViolation(preview)).toBe(false);
    expect(hasMediaSrcViolation(objectUrl)).toBe(false);
  });

  it('allows a literal absolute src (no media-field expression)', async () => {
    const messages = await lintRenderer(`
      export const H = () => <img src="https://cdn.example/x.png" alt="" />;
    `);
    expect(hasMediaSrcViolation(messages)).toBe(false);
  });
});
