import type { Schema } from 'hast-util-sanitize';

/**
 * Strict rehype-sanitize schema for rendered Markdown.
 *
 * This schema is the sole XSS defense for content rendered through the
 * Markdown pipeline. Any relaxation of these rules must be reviewed as a
 * security-sensitive change — see [internal] AI code generation constraints.
 *
 * Allowed semantics:
 * - Basic block + inline formatting (p, strong, em, del, code, pre, ...).
 * - `a[href]` with http / https / mailto protocols only.
 * - `code[class]` limited to highlight.js (`hljs-*`) and language tags
 *   (`language-*`), emitted by rehype-highlight.
 * - `span[class]` limited to a short allowlist used by our own renderers
 *   (emoji, spoiler, mention highlight).
 *
 * Everything else — images, iframes, media, scripts, styles, forms, event
 * handlers, custom protocols (javascript:, data:, file:, blob:, vbscript:) —
 * is stripped.
 */

const ALLOWED_PROTOCOLS = ['http', 'https', 'mailto'];

// Span classes that our renderers can emit. Each class is matched
// individually against className tokens (hast-util-sanitize splits
// whitespace-separated className values before checking).
const SPAN_CLASS_ALLOWLIST = ['emoji', 'spoiler', 'spoiler-revealed', 'mention-highlight'];

// Matches either the bare `hljs` theme anchor OR `hljs-<identifier>` token
// classes. rehype-highlight emits BOTH: the outer `<code>` gets
// `class="hljs language-xxx"` (bare `hljs` is the CSS theme hook that
// provides background + default colors), and inner `<span>` nodes get
// per-token classes like `hljs-keyword`, `hljs-string`, etc. The sanitizer
// validates each whitespace-separated class token individually, so both
// variants must be accepted to avoid silently stripping either.
const HLJS_TOKEN_PATTERN = String.raw`hljs(?:-[\w-]+)?`;

// Matches a single class token on `<code>`: the highlight.js token classes
// (bare `hljs` or `hljs-<x>`) or the language tag (`language-<x>`). Anchored
// to start+end so partial matches are rejected.
const CODE_CLASS_PATTERN = new RegExp(String.raw`^(?:${HLJS_TOKEN_PATTERN}|language-[\w-]+)$`);

// Matches a single class token on `<span>`: our semantic allowlist OR the
// highlight.js per-token classes that rehype-highlight emits inside fenced
// code blocks. Without the hljs-* branch, syntax highlighting silently
// strips all token colors.
const SPAN_CLASS_PATTERN = new RegExp(
  '^(?:' + [...SPAN_CLASS_ALLOWLIST, HLJS_TOKEN_PATTERN].join('|') + ')$'
);

// Matches a mention-token value emitted by remarkMentionTokens as the
// `data-mention` attribute on mention-highlight spans. Supports both the
// UUID-style token format (<@userId>, <@&roleId>) and the plain @-handle
// format (@alice, @all, @here). Anchored so no other attribute content
// can reach the DOM.
const MENTION_TOKEN_PATTERN = /^(?:<@&?[\w-]+>|@[\w.-]+)$/;
const ORDERED_LIST_START_PATTERN = /^\d+$/;

export const sanitizeSchema: Schema = {
  tagNames: [
    'p',
    'br',
    'a',
    'strong',
    'em',
    'del',
    'code',
    'pre',
    'blockquote',
    'ul',
    'ol',
    'li',
    'h1',
    'h2',
    'h3',
    'hr',
    'span',
  ],
  attributes: {
    a: ['href', 'title'],
    ol: [['start', ORDERED_LIST_START_PATTERN]],
    code: [['className', CODE_CLASS_PATTERN]],
    span: [
      ['className', SPAN_CLASS_PATTERN],
      ['dataMention', MENTION_TOKEN_PATTERN],
    ],
  },
  protocols: {
    href: ALLOWED_PROTOCOLS,
  },
  // Tags explicitly stripped (content removed as well, rather than unwrapped).
  // Anything not in tagNames is unwrapped by default; these are the ones where
  // we do not want the inner content to survive.
  strip: [
    'img',
    'iframe',
    'video',
    'audio',
    'embed',
    'object',
    'svg',
    'math',
    'script',
    'style',
    'link',
    'meta',
    'form',
    'input',
    'button',
    'textarea',
    'select',
    'canvas',
    'noscript',
    'base',
    'applet',
    'marquee',
    'details',
    'summary',
  ],
  clobberPrefix: 'md-',
  clobber: ['name', 'id'],
  allowComments: false,
  allowDoctypes: false,
};
