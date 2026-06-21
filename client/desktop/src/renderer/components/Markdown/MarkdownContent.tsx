import React, { useEffect, useMemo } from 'react';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import rehypeSanitize from 'rehype-sanitize';
import rehypeHighlight from 'rehype-highlight';
import remarkSpoiler from './plugins/remarkSpoiler';
import remarkEmojiShortcodes from './plugins/remarkEmojiShortcodes';
import remarkMentionTokens from './plugins/remarkMentionTokens';
import rehypeInlineEmoji from './plugins/rehypeInlineEmoji';
import { sanitizeSchema } from './sanitizeSchema';
import SafeLink from './SafeLink';
import Spoiler from './Spoiler';
import MentionChip from './MentionChip';
import { loadHighlightTheme } from './highlightTheme';
import { useSettingsStore } from '@/renderer/stores/settingsStore';
import type { MentionLookup } from './mentionTypes';
import './MarkdownContent.css';

/**
 * MarkdownContent — memoized core pipeline that renders a Markdown string
 * through remark (GFM + Concord custom plugins) → rehype-sanitize → rehype-highlight
 * and then into React via react-markdown v9.
 *
 * Security: rehype-sanitize with our strict schema is the single XSS defense;
 * SafeLink is defense-in-depth for href protocols. No raw HTML passes through.
 *
 * Memoization: `propsEqual` ensures the entire plugin pipeline only re-runs
 * when `id`, `content`, `editedAt`, or `mentionLookup` actually change. Chat
 * transcripts re-render frequently and every avoided pipeline run is a win.
 */

interface MarkdownContentProps {
  id: string;
  content: string;
  editedAt: string | null | undefined;
  mentionLookup: MentionLookup;
}

// Module-scope anchor override. Defined outside the parent component so it
// isn't redeclared every render (Sonar rule: nested component definition).
interface MarkdownAProps {
  href?: string;
  title?: string;
  children?: React.ReactNode;
}
const MarkdownA: React.FC<MarkdownAProps> = ({ href, title, children }) => (
  <SafeLink href={href} title={title}>
    {children}
  </SafeLink>
);

// Factory returning the span override. The closure captures `mentionLookup`
// so MentionChip gets the right resolver; the returned component is declared
// once at module scope, not nested inside MarkdownContent.
function makeMarkdownSpan(
  mentionLookup: MentionLookup
): React.FC<React.HTMLAttributes<HTMLSpanElement>> {
  const MarkdownSpan: React.FC<React.HTMLAttributes<HTMLSpanElement>> = ({
    className,
    children,
    ...props
  }) => {
    if (className === 'spoiler') {
      return <Spoiler>{children}</Spoiler>;
    }
    if (className === 'mention-highlight') {
      // Library asymmetry: the remark plugin emits hProperties.dataMention (camelCase)
      // and the sanitize schema allowlists 'dataMention'. BUT hast-util-to-jsx-runtime
      // reads property-information's info.attribute for data-* properties, which is
      // always kebab-case — so React receives the prop literally keyed 'data-mention'.
      // DO NOT change this to `dataMention`; the JSX input from react-markdown uses kebab.
      const token = (props as { 'data-mention'?: string })['data-mention'];
      if (token) {
        return <MentionChip token={token} lookup={mentionLookup} />;
      }
    }
    return <span className={className}>{children}</span>;
  };
  MarkdownSpan.displayName = 'MarkdownSpan';
  return MarkdownSpan;
}

const MarkdownContentInner: React.FC<MarkdownContentProps> = ({
  id: _id,
  content,
  editedAt: _editedAt,
  mentionLookup,
}) => {
  const theme = useSettingsStore((s) => s.appearance.theme);

  useEffect(() => {
    const resolved: 'light' | 'dark' = theme === 'light' ? 'light' : 'dark';
    loadHighlightTheme(resolved);
  }, [theme]);

  const components = useMemo(
    () => ({
      a: MarkdownA,
      span: makeMarkdownSpan(mentionLookup),
    }),
    [mentionLookup]
  );

  return (
    <div className="markdown-content">
      <ReactMarkdown
        remarkPlugins={[remarkGfm, remarkSpoiler, remarkEmojiShortcodes, remarkMentionTokens]}
        rehypePlugins={[rehypeHighlight, rehypeInlineEmoji, [rehypeSanitize, sanitizeSchema]]}
        components={components}
      >
        {content}
      </ReactMarkdown>
    </div>
  );
};

function propsEqual(prev: MarkdownContentProps, next: MarkdownContentProps): boolean {
  return (
    prev.id === next.id &&
    prev.content === next.content &&
    prev.editedAt === next.editedAt &&
    prev.mentionLookup === next.mentionLookup
  );
}

const MarkdownContent = React.memo(MarkdownContentInner, propsEqual);
MarkdownContent.displayName = 'MarkdownContent';

export default MarkdownContent;
