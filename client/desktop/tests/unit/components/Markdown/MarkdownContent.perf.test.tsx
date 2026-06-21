import { describe, it, expect, beforeEach } from 'vitest';
import { render } from '../../../test-utils';
import MarkdownContent from '@/renderer/components/Markdown/MarkdownContent';
import { resetAllStores } from '../../../helpers/store-helpers';

const emptyLookup = { users: new Map(), roles: new Map() };

/** Constructs a pathological Markdown string of the target byte length.
 *  24,000 bytes is a perf-regression canary — a large but still-renderable
 *  input. After the message-length policy raise (backend ciphertext cap now
 *  65,536 / inline-renderable .md cap 256 KiB), 24,000 is no longer "the cap"
 *  but remains a useful canary for renderer regressions.
 *  Mix of block constructs: nested quotes, lists, bold, inline code, links. */
function buildWorstCaseMarkdown(targetLen: number): string {
  const parts: string[] = [];
  let len = 0;
  let depth = 0;
  while (len < targetLen - 200) {
    const line =
      depth % 2 === 0
        ? `> **level-${depth}** nested *quote* with \`code\` and [link](https://example.com) and more text to fill the line.\n`
        : `${'  '.repeat(depth)}- item at depth ${depth} with **bold**\n`;
    parts.push(line);
    len += line.length;
    depth = (depth + 1) % 5;
  }
  return parts.join('');
}

describe('MarkdownContent perf', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('renders 24000-byte pathological markdown under budget', () => {
    const content = buildWorstCaseMarkdown(24000);
    expect(content.length).toBeGreaterThan(20000);
    expect(content.length).toBeLessThanOrEqual(24000);

    const start = performance.now();
    render(
      <MarkdownContent id="perf" content={content} editedAt={null} mentionLookup={emptyLookup} />
    );
    const elapsed = performance.now() - start;

    // Hard bound: 1500ms. This is a regression detector, not a perf enforcer.
    // Measured baselines:
    //   Local Apple Silicon:  ~115ms solo, ~135ms in-suite
    //   GitHub Actions Ubuntu: ~525ms (first observation PR #711)
    // jsdom inflates DOM ops vs real browsers by roughly 5-10x, and CI
    // runners are an additional 3-4x slower than local dev machines for
    // jsdom workloads. The 1500ms bound accommodates both variables while
    // still catching a ~3x regression from the current CI baseline — a
    // 3x slowdown would be a genuine O(n²) or infinite-loop regression,
    // not a noise fluctuation.
    //
    // The real performance target (<16ms on production hardware for the
    // hot render path) is tracked via the informational console.warn
    // below — NOT via this assertion. A follow-up benchmark suite using
    // happy-dom or a real headless browser could measure that target
    // accurately if ever needed.
    expect(elapsed).toBeLessThan(1500);

    if (elapsed > 16) {
      console.warn(
        `MarkdownContent perf: ${elapsed.toFixed(2)}ms for 24000-byte input (target <16ms)`
      );
    }
  });
});
