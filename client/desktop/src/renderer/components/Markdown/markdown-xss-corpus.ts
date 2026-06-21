/** XSS test fixture. 40 payloads drawn from OWASP + PortSwigger lists.
 *  Each payload is fed to <MarkdownContent>; the test assertions verify
 *  no denied tags, no dangerous href protocols, and no event-handler
 *  attributes survive the sanitize pipeline. */
export interface XssPayload {
  name: string;
  input: string;
}

export const XSS_PAYLOADS: XssPayload[] = [
  { name: 'script-tag-inline', input: '<script>alert(1)</script>' },
  { name: 'script-tag-inside-bold', input: '**<script>alert(1)</script>**' },
  { name: 'script-tag-inside-code', input: '`<script>alert(1)</script>`' },
  { name: 'img-onerror', input: '<img src=x onerror=alert(1)>' },
  { name: 'iframe-src', input: '<iframe src="javascript:alert(1)"></iframe>' },
  { name: 'svg-onload', input: '<svg onload=alert(1)></svg>' },
  { name: 'math-href', input: '<math><mtext><a href="javascript:alert(1)">x</a></mtext></math>' },
  { name: 'details-open', input: '<details open ontoggle=alert(1)>x</details>' },
  { name: 'link-javascript', input: '[x](javascript:alert(1))' },
  { name: 'link-data-html', input: '[x](data:text/html,abc)' },
  { name: 'link-file', input: '[x](file:///etc/passwd)' },
  { name: 'link-blob', input: '[x](blob:http://x/1)' },
  { name: 'link-vbscript', input: '[x](vbscript:msgbox(1))' },
  { name: 'link-javascript-uppercase', input: '[x](JaVaScRiPt:alert(1))' },
  { name: 'link-javascript-entities', input: '[x](&#106;avascript:alert(1))' },
  { name: 'img-markdown', input: '![alt](http://example.com/evil.png)' },
  { name: 'title-injection', input: '[x](http://example.com "onclick=alert(1)")' },
  { name: 'href-with-newline', input: '[x](http://example.com\n<script>alert(1)</script>)' },
  { name: 'style-tag', input: '<style>body{background:red}</style>' },
  { name: 'form-post', input: '<form action="http://evil" method="post"></form>' },
  { name: 'input-type', input: '<input type="text" name="x">' },
  { name: 'button-onclick', input: '<button onclick="alert(1)">x</button>' },
  { name: 'object-data', input: '<object data="javascript:alert(1)"></object>' },
  { name: 'embed-src', input: '<embed src="javascript:alert(1)">' },
  { name: 'video-onerror', input: '<video onerror=alert(1) src=x></video>' },
  { name: 'audio-onplay', input: '<audio onplay=alert(1) src=x autoplay></audio>' },
  { name: 'base-href', input: '<base href="javascript:alert(1)//">' },
  { name: 'meta-refresh', input: '<meta http-equiv="refresh" content="0;javascript:alert(1)">' },
  { name: 'link-stylesheet', input: '<link rel="stylesheet" href="javascript:alert(1)">' },
  { name: 'noscript-tag', input: '<noscript>&lt;script&gt;alert(1)&lt;/script&gt;</noscript>' },
  { name: 'html-comment-escape', input: '<!--<script>alert(1)//-->' },
  { name: 'applet-code', input: '<applet code="Evil"></applet>' },
  { name: 'marquee-onstart', input: '<marquee onstart=alert(1)>x</marquee>' },
  { name: 'textarea-autofocus', input: '<textarea autofocus onfocus=alert(1)>' },
  {
    name: 'select-autofocus',
    input: '<select autofocus onfocus=alert(1)><option>x</option></select>',
  },
  { name: 'canvas-tag', input: '<canvas id=x></canvas>' },
  { name: 'span-class-evil', input: '<span class="evil">x</span>' },
  { name: 'anchor-target-blank-xss', input: '<a href="javascript:alert(1)" target=_blank>x</a>' },
  { name: 'url-unicode-trick', input: '[x](java\u0073cript:alert(1))' },
  { name: 'dbquote-break', input: '[x](http://example.com" onclick="alert(1))' },
];
