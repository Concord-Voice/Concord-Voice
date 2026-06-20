# NOTICE — Third-Party Software Used by Concord Voice

**Generated:** 2026-05-31
**Concord Voice License:** [Concord Voice Source License 1.0 (CVSL 1.0)](./LICENSE) → AGPL-3.0-or-later on 2030-02-15
**Audit reference:** [docs/legal/dependency-license-audit.md](docs/legal/dependency-license-audit.md)

This NOTICE file lists the third-party open-source software used by Concord
Voice and acknowledges the copyright holders of that software, as required by
each component's license. Concord Voice LLC gratefully acknowledges the
contributions of these projects to the Concord Voice platform.

## Summary

Concord Voice distributes 391 third-party software components across its
artifacts:

- **55** Go modules in the control-plane runtime binary
- **187** npm packages in the desktop client production bundle (plus Electron and its bundled components)
- **149** npm packages in the media-plane production bundle

(Counts exclude Concord Voice's own private packages — `@concordvoice/desktop`,
`@concordvoice/media-plane` — and the local stub override
`node-domexception-stub`. Those are first-party components, not third-party
attributions.)

License distribution:

| License family | Approximate share | Compatible with CVSL 1.0 + AGPL-3.0 |
|---|---|---|
| MIT | ~75% | ✓ |
| ISC | ~10% | ✓ |
| Apache-2.0 | ~7% | ✓ |
| BSD-2-Clause / BSD-3-Clause | ~6% | ✓ |
| MPL-2.0 (build-time only) | <1% | ✓ |
| Other permissive (BlueOak-1.0.0, 0BSD, Python-2.0, Unlicense, CC0-1.0, MIT-0) | <2% | ✓ |
| CC-BY-3.0 / CC-BY-4.0 (build-data only) | <1% | ✓ (attribution required) |

No GPL, AGPL, SSPL, BUSL, Elastic License, Commons Clause, or other
copyleft / non-commercial licenses are used in any distributed artifact.

## License Texts

The license texts for each license family are available at the canonical
SPDX URLs:

- **MIT** — [https://opensource.org/license/mit](https://opensource.org/license/mit)
- **ISC** — [https://opensource.org/license/isc](https://opensource.org/license/isc)
- **Apache-2.0** — [https://www.apache.org/licenses/LICENSE-2.0](https://www.apache.org/licenses/LICENSE-2.0)
- **BSD-2-Clause** — [https://opensource.org/license/bsd-2-clause](https://opensource.org/license/bsd-2-clause)
- **BSD-3-Clause** — [https://opensource.org/license/bsd-3-clause](https://opensource.org/license/bsd-3-clause)
- **0BSD** — [https://opensource.org/license/0bsd](https://opensource.org/license/0bsd)
- **MPL-2.0** — [https://www.mozilla.org/en-US/MPL/2.0/](https://www.mozilla.org/en-US/MPL/2.0/)
- **BlueOak-1.0.0** — [https://blueoakcouncil.org/license/1.0.0](https://blueoakcouncil.org/license/1.0.0)
- **Unlicense** — [https://unlicense.org/](https://unlicense.org/)
- **Python-2.0** — [https://docs.python.org/3/license.html](https://docs.python.org/3/license.html)
- **CC0-1.0** — [https://creativecommons.org/publicdomain/zero/1.0/](https://creativecommons.org/publicdomain/zero/1.0/)
- **CC-BY-3.0** — [https://creativecommons.org/licenses/by/3.0/](https://creativecommons.org/licenses/by/3.0/)
- **CC-BY-4.0** — [https://creativecommons.org/licenses/by/4.0/](https://creativecommons.org/licenses/by/4.0/)

Individual project copyright notices and full license texts are preserved
in the redistributed source / binary form (npm `node_modules`, Go module
cache, Electron credits at `about:credits` in the application). For
projects that ship their own NOTICE file (Apache-2.0 § 4 requirement),
that content is included with the project in its `node_modules` directory
or Go module cache and is not duplicated here.

## Electron and Chromium

The Concord Voice desktop application is built on Electron and embeds
Chromium. Electron itself is licensed under MIT. Chromium and its
components are licensed under multiple compatible licenses (predominantly
BSD-3-Clause, with various permissive licenses for individual components).
The full Chromium credits are included in the desktop application
installer and accessible at runtime via the in-app About section. The
Electron and Chromium teams are gratefully acknowledged.

- **Electron** — [https://www.electronjs.org/](https://www.electronjs.org/) — MIT License
- **Chromium** — [https://www.chromium.org/](https://www.chromium.org/) — see in-app `about:credits`

---

## Go Control-Plane Runtime Dependencies

The following Go modules are linked into the control-plane runtime binary
(services/control-plane):

| Module | Version | License |
|---|---|---|
| github.com/boombuler/barcode | v1.0.1-0.20190219062509-6c824513bacc | MIT |
| github.com/cespare/xxhash/v2 | v2.3.0 | MIT |
| github.com/dustin/go-humanize | v1.0.1 | MIT |
| github.com/fxamacker/cbor/v2 | v2.9.2 | MIT |
| github.com/gabriel-vasile/mimetype | v1.4.12 | MIT |
| github.com/gin-contrib/sse | v1.1.0 | MIT |
| github.com/gin-gonic/gin | v1.12.0 | MIT |
| github.com/go-playground/locales | v0.14.1 | MIT |
| github.com/go-playground/universal-translator | v0.18.1 | MIT |
| github.com/go-playground/validator/v10 | v10.30.1 | MIT |
| github.com/go-viper/mapstructure/v2 | v2.5.0 | MIT |
| github.com/go-webauthn/webauthn | v0.17.4 | BSD-3-Clause |
| github.com/go-webauthn/x | v0.2.6 | BSD-3-Clause |
| github.com/goccy/go-yaml | v1.19.2 | MIT |
| github.com/golang-jwt/jwt/v5 | v5.3.1 | MIT |
| github.com/golang-migrate/migrate/v4 | v4.19.1 | MIT |
| github.com/google/go-tpm | v0.9.8 | Apache-2.0 |
| github.com/google/uuid | v1.6.0 | BSD-3-Clause |
| github.com/gorilla/websocket | v1.5.3 | BSD-2-Clause |
| github.com/joho/godotenv | v1.5.1 | MIT |
| github.com/klauspost/compress | v1.18.6 | MIT |
| github.com/klauspost/cpuid/v2 | v2.3.0 | MIT |
| github.com/klauspost/crc32 | v1.3.0 | BSD-3-Clause |
| github.com/leodido/go-urn | v1.4.0 | MIT |
| github.com/lib/pq | v1.12.3 | MIT |
| github.com/mattn/go-isatty | v0.0.20 | MIT |
| github.com/minio/crc64nvme | v1.1.1 | Apache-2.0 |
| github.com/minio/md5-simd | v1.1.2 | Apache-2.0 |
| github.com/minio/minio-go/v7 | v7.2.0 | Apache-2.0 |
| github.com/nats-io/nats.go | v1.52.0 | Apache-2.0 |
| github.com/nats-io/nkeys | v0.4.15 | Apache-2.0 |
| github.com/nats-io/nuid | v1.0.1 | Apache-2.0 |
| github.com/pelletier/go-toml/v2 | v2.3.1 | MIT |
| github.com/philhofer/fwd | v1.2.0 | MIT |
| github.com/pquerna/otp | v1.5.0 | Apache-2.0 |
| github.com/quic-go/qpack | v0.6.0 | MIT |
| github.com/quic-go/quic-go | v0.59.0 | MIT |
| github.com/redis/go-redis/v9 | v9.19.0 | BSD-2-Clause |
| github.com/rs/xid | v1.6.0 | MIT |
| github.com/tinylib/msgp | v1.6.4 | MIT |
| github.com/ugorji/go/codec | v1.3.1 | MIT |
| github.com/vmihailenco/msgpack/v5 | v5.4.1 | BSD-2-Clause |
| github.com/vmihailenco/tagparser/v2 | v2.0.0 | BSD-2-Clause |
| github.com/x448/float16 | v0.8.4 | MIT |
| github.com/zeebo/xxh3 | v1.1.0 | BSD-2-Clause |
| go.mongodb.org/mongo-driver/v2 | v2.5.0 | Apache-2.0 |
| go.uber.org/atomic | v1.11.0 | MIT |
| go.yaml.in/yaml/v3 | v3.0.4 | MIT |
| golang.org/x/crypto | v0.52.0 | BSD-3-Clause |
| golang.org/x/image | v0.41.0 | BSD-3-Clause |
| golang.org/x/net | v0.54.0 | BSD-3-Clause |
| golang.org/x/sys | v0.45.0 | BSD-3-Clause |
| golang.org/x/text | v0.37.0 | BSD-3-Clause |
| google.golang.org/protobuf | v1.36.11 | BSD-3-Clause |
| gopkg.in/ini.v1 | v1.67.2 | Apache-2.0 |

---

## Desktop Client Production Dependencies

The following npm packages are included in the desktop client production
bundle (client/desktop):

| Package | Version | License | Repository |
|---|---|---|---|
| @lukeed/csprng | 1.1.0 | MIT | https://github.com/lukeed/csprng |
| @lukeed/uuid | 2.0.1 | MIT | https://github.com/lukeed/uuid |
| @msgpack/msgpack | 3.1.3 | ISC | https://github.com/msgpack/msgpack-javascript |
| @noble/hashes | 2.2.0 | MIT | https://github.com/paulmillr/noble-hashes |
| @socket.io/component-emitter | 3.1.2 | MIT | https://github.com/socketio/emitter |
| @types/debug | 4.1.13 | MIT | https://github.com/DefinitelyTyped/DefinitelyTyped |
| @types/estree-jsx | 1.0.5 | MIT | https://github.com/DefinitelyTyped/DefinitelyTyped |
| @types/estree | 1.0.8 | MIT | https://github.com/DefinitelyTyped/DefinitelyTyped |
| @types/events | 3.0.3 | MIT | https://github.com/DefinitelyTyped/DefinitelyTyped |
| @types/hast | 3.0.4 | MIT | https://github.com/DefinitelyTyped/DefinitelyTyped |
| @types/mdast | 4.0.4 | MIT | https://github.com/DefinitelyTyped/DefinitelyTyped |
| @types/ms | 2.1.0 | MIT | https://github.com/DefinitelyTyped/DefinitelyTyped |
| @types/react | 19.2.15 | MIT | https://github.com/DefinitelyTyped/DefinitelyTyped |
| @types/unist | 2.0.11 | MIT | https://github.com/DefinitelyTyped/DefinitelyTyped |
| @types/unist | 3.0.3 | MIT | https://github.com/DefinitelyTyped/DefinitelyTyped |
| @ungap/structured-clone | 1.3.0 | ISC | https://github.com/ungap/structured-clone |
| ansi-regex | 5.0.1 | MIT | https://github.com/chalk/ansi-regex |
| ansi-styles | 4.3.0 | MIT | https://github.com/chalk/ansi-styles |
| argparse | 2.0.1 | Python-2.0 | https://github.com/nodeca/argparse |
| awaitqueue | 3.3.0 | ISC | https://github.com/versatica/awaitqueue |
| bail | 2.0.2 | MIT | https://github.com/wooorm/bail |
| builder-util-runtime | 9.5.1 | MIT | https://github.com/electron-userland/electron-builder |
| camelcase | 5.3.1 | MIT | https://github.com/sindresorhus/camelcase |
| ccount | 2.0.1 | MIT | https://github.com/wooorm/ccount |
| character-entities-html4 | 2.1.0 | MIT | https://github.com/wooorm/character-entities-html4 |
| character-entities-legacy | 3.0.0 | MIT | https://github.com/wooorm/character-entities-legacy |
| character-entities | 2.0.2 | MIT | https://github.com/wooorm/character-entities |
| character-reference-invalid | 2.0.1 | MIT | https://github.com/wooorm/character-reference-invalid |
| cliui | 6.0.0 | ISC | https://github.com/yargs/cliui |
| color-convert | 2.0.1 | MIT | https://github.com/Qix-/color-convert |
| color-name | 1.1.4 | MIT | https://github.com/colorjs/color-name |
| comma-separated-tokens | 2.0.3 | MIT | https://github.com/wooorm/comma-separated-tokens |
| cookie | 1.1.1 | MIT | https://github.com/jshttp/cookie |
| csstype | 3.2.3 | MIT | https://github.com/frenic/csstype |
| debug | 2.6.9 | MIT | https://github.com/visionmedia/debug |
| debug | 4.4.3 | MIT | https://github.com/debug-js/debug |
| decamelize | 1.2.0 | MIT | https://github.com/sindresorhus/decamelize |
| decode-named-character-reference | 1.3.0 | MIT | https://github.com/wooorm/decode-named-character-reference |
| dequal | 2.0.3 | MIT | https://github.com/lukeed/dequal |
| devlop | 1.1.0 | MIT | https://github.com/wooorm/devlop |
| dijkstrajs | 1.0.3 | MIT | https://github.com/tcort/dijkstrajs |
| electron-squirrel-startup | 1.0.1 | Apache-2.0 | https://github.com/mongodb-js/electron-squirrel-startup |
| electron-updater | 6.8.3 | MIT | https://github.com/electron-userland/electron-builder |
| emoji-regex | 8.0.0 | MIT | https://github.com/mathiasbynens/emoji-regex |
| engine.io-client | 6.6.5 | MIT | https://github.com/socketio/socket.io |
| engine.io-parser | 5.2.3 | MIT | https://github.com/socketio/socket.io |
| escape-string-regexp | 5.0.0 | MIT | https://github.com/sindresorhus/escape-string-regexp |
| estree-util-is-identifier-name | 3.0.0 | MIT | https://github.com/syntax-tree/estree-util-is-identifier-name |
| events | 3.3.0 | MIT | https://github.com/Gozala/events |
| extend | 3.0.2 | MIT | https://github.com/justmoon/node-extend |
| fake-mediastreamtrack | 2.2.1 | ISC | https://github.com/ibc/fake-mediastreamtrack |
| find-up | 4.1.0 | MIT | https://github.com/sindresorhus/find-up |
| fs-extra | 10.1.0 | MIT | https://github.com/jprichardson/node-fs-extra |
| get-caller-file | 2.0.5 | ISC | https://github.com/stefanpenner/get-caller-file |
| graceful-fs | 4.2.11 | ISC | https://github.com/isaacs/node-graceful-fs |
| h264-profile-level-id | 2.3.2 | ISC | https://github.com/versatica/h264-profile-level-id |
| hash-wasm | 4.12.0 | MIT | https://github.com/Daninet/hash-wasm |
| hast-util-is-element | 3.0.0 | MIT | https://github.com/syntax-tree/hast-util-is-element |
| hast-util-sanitize | 5.0.2 | MIT | https://github.com/syntax-tree/hast-util-sanitize |
| hast-util-to-jsx-runtime | 2.3.6 | MIT | https://github.com/syntax-tree/hast-util-to-jsx-runtime |
| hast-util-to-text | 4.0.2 | MIT | https://github.com/syntax-tree/hast-util-to-text |
| hast-util-whitespace | 3.0.0 | MIT | https://github.com/syntax-tree/hast-util-whitespace |
| highlight.js | 11.11.1 | BSD-3-Clause | https://github.com/highlightjs/highlight.js |
| html-url-attributes | 3.0.1 | MIT | https://github.com/rehypejs/rehype-minify/tree/main/packages/html-url-attributes |
| inline-style-parser | 0.2.7 | MIT | https://github.com/remarkablemark/inline-style-parser |
| is-alphabetical | 2.0.1 | MIT | https://github.com/wooorm/is-alphabetical |
| is-alphanumerical | 2.0.1 | MIT | https://github.com/wooorm/is-alphanumerical |
| is-decimal | 2.0.1 | MIT | https://github.com/wooorm/is-decimal |
| is-fullwidth-code-point | 3.0.0 | MIT | https://github.com/sindresorhus/is-fullwidth-code-point |
| is-hexadecimal | 2.0.1 | MIT | https://github.com/wooorm/is-hexadecimal |
| is-plain-obj | 4.1.0 | MIT | https://github.com/sindresorhus/is-plain-obj |
| js-yaml | 4.1.1 | MIT | https://github.com/nodeca/js-yaml |
| jsonfile | 6.2.0 | MIT | https://github.com/jprichardson/node-jsonfile |
| lazy-val | 1.0.5 | MIT | https://github.com/develar/lazy-val |
| locate-path | 5.0.0 | MIT | https://github.com/sindresorhus/locate-path |
| lodash.escaperegexp | 4.1.2 | MIT | https://github.com/lodash/lodash |
| lodash.isequal | 4.5.0 | MIT | https://github.com/lodash/lodash |
| longest-streak | 3.1.0 | MIT | https://github.com/wooorm/longest-streak |
| lowlight | 3.3.0 | MIT | https://github.com/wooorm/lowlight |
| lucide-react | 1.16.0 | ISC | https://github.com/lucide-icons/lucide |
| markdown-table | 3.0.4 | MIT | https://github.com/wooorm/markdown-table |
| mdast-util-find-and-replace | 3.0.2 | MIT | https://github.com/syntax-tree/mdast-util-find-and-replace |
| mdast-util-from-markdown | 2.0.3 | MIT | https://github.com/syntax-tree/mdast-util-from-markdown |
| mdast-util-gfm-autolink-literal | 2.0.1 | MIT | https://github.com/syntax-tree/mdast-util-gfm-autolink-literal |
| mdast-util-gfm-footnote | 2.1.0 | MIT | https://github.com/syntax-tree/mdast-util-gfm-footnote |
| mdast-util-gfm-strikethrough | 2.0.0 | MIT | https://github.com/syntax-tree/mdast-util-gfm-strikethrough |
| mdast-util-gfm-table | 2.0.0 | MIT | https://github.com/syntax-tree/mdast-util-gfm-table |
| mdast-util-gfm-task-list-item | 2.0.0 | MIT | https://github.com/syntax-tree/mdast-util-gfm-task-list-item |
| mdast-util-gfm | 3.1.0 | MIT | https://github.com/syntax-tree/mdast-util-gfm |
| mdast-util-mdx-expression | 2.0.1 | MIT | https://github.com/syntax-tree/mdast-util-mdx-expression |
| mdast-util-mdx-jsx | 3.2.0 | MIT | https://github.com/syntax-tree/mdast-util-mdx-jsx |
| mdast-util-mdxjs-esm | 2.0.1 | MIT | https://github.com/syntax-tree/mdast-util-mdxjs-esm |
| mdast-util-phrasing | 4.1.0 | MIT | https://github.com/syntax-tree/mdast-util-phrasing |
| mdast-util-to-hast | 13.2.1 | MIT | https://github.com/syntax-tree/mdast-util-to-hast |
| mdast-util-to-markdown | 2.1.2 | MIT | https://github.com/syntax-tree/mdast-util-to-markdown |
| mdast-util-to-string | 4.0.0 | MIT | https://github.com/syntax-tree/mdast-util-to-string |
| mediasoup-client | 3.20.0 | ISC | https://github.com/versatica/mediasoup-client |
| micromark-core-commonmark | 2.0.3 | MIT | https://github.com/micromark/micromark/tree/main/packages/micromark-core-commonmark |
| micromark-extension-gfm-autolink-literal | 2.1.0 | MIT | https://github.com/micromark/micromark-extension-gfm-autolink-literal |
| micromark-extension-gfm-footnote | 2.1.0 | MIT | https://github.com/micromark/micromark-extension-gfm-footnote |
| micromark-extension-gfm-strikethrough | 2.1.0 | MIT | https://github.com/micromark/micromark-extension-gfm-strikethrough |
| micromark-extension-gfm-table | 2.1.1 | MIT | https://github.com/micromark/micromark-extension-gfm-table |
| micromark-extension-gfm-tagfilter | 2.0.0 | MIT | https://github.com/micromark/micromark-extension-gfm-tagfilter |
| micromark-extension-gfm-task-list-item | 2.1.0 | MIT | https://github.com/micromark/micromark-extension-gfm-task-list-item |
| micromark-extension-gfm | 3.0.0 | MIT | https://github.com/micromark/micromark-extension-gfm |
| micromark-factory-destination | 2.0.1 | MIT | https://github.com/micromark/micromark/tree/main/packages/micromark-factory-destination |
| micromark-factory-label | 2.0.1 | MIT | https://github.com/micromark/micromark/tree/main/packages/micromark-factory-label |
| micromark-factory-space | 2.0.1 | MIT | https://github.com/micromark/micromark/tree/main/packages/micromark-factory-space |
| micromark-factory-title | 2.0.1 | MIT | https://github.com/micromark/micromark/tree/main/packages/micromark-factory-title |
| micromark-factory-whitespace | 2.0.1 | MIT | https://github.com/micromark/micromark/tree/main/packages/micromark-factory-whitespace |
| micromark-util-character | 2.1.1 | MIT | https://github.com/micromark/micromark/tree/main/packages/micromark-util-character |
| micromark-util-chunked | 2.0.1 | MIT | https://github.com/micromark/micromark/tree/main/packages/micromark-util-chunked |
| micromark-util-classify-character | 2.0.1 | MIT | https://github.com/micromark/micromark/tree/main/packages/micromark-util-classify-character |
| micromark-util-combine-extensions | 2.0.1 | MIT | https://github.com/micromark/micromark/tree/main/packages/micromark-util-combine-extensions |
| micromark-util-decode-numeric-character-reference | 2.0.2 | MIT | https://github.com/micromark/micromark/tree/main/packages/micromark-util-decode-numeric-character-reference |
| micromark-util-decode-string | 2.0.1 | MIT | https://github.com/micromark/micromark/tree/main/packages/micromark-util-decode-string |
| micromark-util-encode | 2.0.1 | MIT | https://github.com/micromark/micromark/tree/main/packages/micromark-util-encode |
| micromark-util-html-tag-name | 2.0.1 | MIT | https://github.com/micromark/micromark/tree/main/packages/micromark-util-html-tag-name |
| micromark-util-normalize-identifier | 2.0.1 | MIT | https://github.com/micromark/micromark/tree/main/packages/micromark-util-normalize-identifier |
| micromark-util-resolve-all | 2.0.1 | MIT | https://github.com/micromark/micromark/tree/main/packages/micromark-util-resolve-all |
| micromark-util-sanitize-uri | 2.0.1 | MIT | https://github.com/micromark/micromark/tree/main/packages/micromark-util-sanitize-uri |
| micromark-util-subtokenize | 2.1.0 | MIT | https://github.com/micromark/micromark/tree/main/packages/micromark-util-subtokenize |
| micromark-util-symbol | 2.0.1 | MIT | https://github.com/micromark/micromark/tree/main/packages/micromark-util-symbol |
| micromark-util-types | 2.0.2 | MIT | https://github.com/micromark/micromark/tree/main/packages/micromark-util-types |
| micromark | 4.0.2 | MIT | https://github.com/micromark/micromark/tree/main/packages/micromark |
| minisearch | 7.2.0 | MIT | https://github.com/lucaong/minisearch |
| ms | 2.0.0 | MIT | https://github.com/zeit/ms |
| ms | 2.1.3 | MIT | https://github.com/vercel/ms |
| p-limit | 2.3.0 | MIT | https://github.com/sindresorhus/p-limit |
| p-locate | 4.1.0 | MIT | https://github.com/sindresorhus/p-locate |
| p-try | 2.2.0 | MIT | https://github.com/sindresorhus/p-try |
| parse-entities | 4.0.2 | MIT | https://github.com/wooorm/parse-entities |
| path-exists | 4.0.0 | MIT | https://github.com/sindresorhus/path-exists |
| pngjs | 5.0.0 | MIT | https://github.com/lukeapage/pngjs |
| property-information | 7.1.0 | MIT | https://github.com/wooorm/property-information |
| qrcode | 1.5.4 | MIT | https://github.com/soldair/node-qrcode |
| react-dom | 19.2.6 | MIT | https://github.com/facebook/react |
| react-markdown | 10.1.0 | MIT | https://github.com/remarkjs/react-markdown |
| react-router-dom | 7.15.1 | MIT | https://github.com/remix-run/react-router |
| react-router | 7.15.1 | MIT | https://github.com/remix-run/react-router |
| react | 19.2.6 | MIT | https://github.com/facebook/react |
| rehype-highlight | 7.0.2 | MIT | https://github.com/rehypejs/rehype-highlight |
| rehype-sanitize | 6.0.0 | MIT | https://github.com/rehypejs/rehype-sanitize |
| remark-gfm | 4.0.1 | MIT | https://github.com/remarkjs/remark-gfm |
| remark-parse | 11.0.0 | MIT | https://github.com/remarkjs/remark/tree/main/packages/remark-parse |
| remark-rehype | 11.1.2 | MIT | https://github.com/remarkjs/remark-rehype |
| remark-stringify | 11.0.0 | MIT | https://github.com/remarkjs/remark/tree/main/packages/remark-stringify |
| require-directory | 2.1.1 | MIT | https://github.com/troygoode/node-require-directory |
| require-main-filename | 2.0.0 | ISC | https://github.com/yargs/require-main-filename |
| sax | 1.6.0 | BlueOak-1.0.0 | https://github.com/isaacs/sax-js |
| scheduler | 0.27.0 | MIT | https://github.com/facebook/react |
| sdp-transform | 3.0.0 | MIT | https://github.com/clux/sdp-transform |
| semver | 7.7.4 | ISC | https://github.com/npm/node-semver |
| set-blocking | 2.0.0 | ISC | https://github.com/yargs/set-blocking |
| set-cookie-parser | 2.7.2 | MIT | https://github.com/nfriedly/set-cookie-parser |
| socket.io-client | 4.8.3 | MIT | https://github.com/socketio/socket.io |
| socket.io-parser | 4.2.6 | MIT | https://github.com/socketio/socket.io |
| space-separated-tokens | 2.0.2 | MIT | https://github.com/wooorm/space-separated-tokens |
| string-width | 4.2.3 | MIT | https://github.com/sindresorhus/string-width |
| stringify-entities | 4.0.4 | MIT | https://github.com/wooorm/stringify-entities |
| strip-ansi | 6.0.1 | MIT | https://github.com/chalk/strip-ansi |
| style-to-js | 1.1.21 | MIT | https://github.com/remarkablemark/style-to-js |
| style-to-object | 1.0.14 | MIT | https://github.com/remarkablemark/style-to-object |
| supports-color | 10.2.2 | MIT | https://github.com/chalk/supports-color |
| tiny-typed-emitter | 2.1.0 | MIT | https://github.com/binier/tiny-typed-emitter |
| trim-lines | 3.0.1 | MIT | https://github.com/wooorm/trim-lines |
| trough | 2.2.0 | MIT | https://github.com/wooorm/trough |
| unified | 11.0.5 | MIT | https://github.com/unifiedjs/unified |
| unist-util-find-after | 5.0.0 | MIT | https://github.com/syntax-tree/unist-util-find-after |
| unist-util-is | 6.0.1 | MIT | https://github.com/syntax-tree/unist-util-is |
| unist-util-position | 5.0.0 | MIT | https://github.com/syntax-tree/unist-util-position |
| unist-util-stringify-position | 4.0.0 | MIT | https://github.com/syntax-tree/unist-util-stringify-position |
| unist-util-visit-parents | 6.0.2 | MIT | https://github.com/syntax-tree/unist-util-visit-parents |
| unist-util-visit | 5.1.0 | MIT | https://github.com/syntax-tree/unist-util-visit |
| universalify | 2.0.1 | MIT | https://github.com/RyanZim/universalify |
| use-sync-external-store | 1.6.0 | MIT | https://github.com/facebook/react |
| vfile-message | 4.0.3 | MIT | https://github.com/vfile/vfile-message |
| vfile | 6.0.3 | MIT | https://github.com/vfile/vfile |
| which-module | 2.0.1 | ISC | https://github.com/nexdrew/which-module |
| wrap-ansi | 6.2.0 | MIT | https://github.com/chalk/wrap-ansi |
| ws | 8.20.1 | MIT | https://github.com/websockets/ws |
| xmlhttprequest-ssl | 2.1.2 | MIT | https://github.com/mjwwit/node-XMLHttpRequest |
| y18n | 4.0.3 | ISC | https://github.com/yargs/y18n |
| yargs-parser | 18.1.3 | ISC | https://github.com/yargs/yargs-parser |
| yargs | 15.4.1 | MIT | https://github.com/yargs/yargs |
| zod | 4.4.3 | MIT | https://github.com/colinhacks/zod |
| zustand | 5.0.13 | MIT | https://github.com/pmndrs/zustand |
| zwitch | 2.0.4 | MIT | https://github.com/wooorm/zwitch |

---

## Media-Plane Production Dependencies

The following npm packages are included in the media-plane production
bundle (services/media-plane):

| Package | Version | License | Repository |
|---|---|---|---|
| @colors/colors | 1.6.0 | MIT | https://github.com/DABH/colors.js |
| @dabh/diagnostics | 2.0.8 | MIT | https://github.com/DABH/diagnostics |
| @isaacs/fs-minipass | 4.0.1 | ISC | https://github.com/npm/fs-minipass |
| @redis/bloom | 5.12.1 | MIT | https://github.com/redis/node-redis |
| @redis/client | 5.12.1 | MIT | https://github.com/redis/node-redis |
| @redis/json | 5.12.1 | MIT | https://github.com/redis/node-redis |
| @redis/search | 5.12.1 | MIT | https://github.com/redis/node-redis |
| @redis/time-series | 5.12.1 | MIT | https://github.com/redis/node-redis |
| @so-ric/colorspace | 1.1.6 | MIT | https://github.com/so-ric/colorspace |
| @socket.io/component-emitter | 3.1.2 | MIT | https://github.com/socketio/emitter |
| @types/cors | 2.8.19 | MIT | https://github.com/DefinitelyTyped/DefinitelyTyped |
| @types/node | 25.9.1 | MIT | https://github.com/DefinitelyTyped/DefinitelyTyped |
| @types/triple-beam | 1.3.5 | MIT | https://github.com/DefinitelyTyped/DefinitelyTyped |
| @types/ws | 8.18.1 | MIT | https://github.com/DefinitelyTyped/DefinitelyTyped |
| accepts | 1.3.8 | MIT | https://github.com/jshttp/accepts |
| accepts | 2.0.0 | MIT | https://github.com/jshttp/accepts |
| async | 3.2.6 | MIT | https://github.com/caolan/async |
| base64id | 2.0.0 | MIT | https://github.com/faeldt/base64id |
| body-parser | 2.2.2 | MIT | https://github.com/expressjs/body-parser |
| buffer-equal-constant-time | 1.0.1 | BSD-3-Clause | https://github.com/goinstant/buffer-equal-constant-time |
| bytes | 3.1.2 | MIT | https://github.com/visionmedia/bytes.js |
| call-bind-apply-helpers | 1.0.2 | MIT | https://github.com/ljharb/call-bind-apply-helpers |
| call-bound | 1.0.4 | MIT | https://github.com/ljharb/call-bound |
| chownr | 3.0.0 | BlueOak-1.0.0 | https://github.com/isaacs/chownr |
| cluster-key-slot | 1.1.2 | Apache-2.0 | https://github.com/Salakar/cluster-key-slot |
| color-convert | 3.1.3 | MIT | https://github.com/Qix-/color-convert |
| color-name | 2.1.0 | MIT | https://github.com/colorjs/color-name |
| color-string | 2.1.4 | MIT | https://github.com/Qix-/color-string |
| color | 5.0.3 | MIT | https://github.com/Qix-/color |
| content-disposition | 1.0.1 | MIT | https://github.com/jshttp/content-disposition |
| content-type | 1.0.5 | MIT | https://github.com/jshttp/content-type |
| cookie-signature | 1.2.2 | MIT | https://github.com/visionmedia/node-cookie-signature |
| cookie | 0.7.2 | MIT | https://github.com/jshttp/cookie |
| cors | 2.8.6 | MIT | https://github.com/expressjs/cors |
| data-uri-to-buffer | 4.0.1 | MIT | https://github.com/TooTallNate/node-data-uri-to-buffer |
| debug | 4.4.3 | MIT | https://github.com/debug-js/debug |
| depd | 2.0.0 | MIT | https://github.com/dougwilson/nodejs-depd |
| dotenv | 17.4.2 | BSD-2-Clause | https://github.com/motdotla/dotenv |
| dunder-proto | 1.0.1 | MIT | https://github.com/es-shims/dunder-proto |
| ecdsa-sig-formatter | 1.0.11 | Apache-2.0 | https://github.com/Brightspace/node-ecdsa-sig-formatter |
| ee-first | 1.1.1 | MIT | https://github.com/jonathanong/ee-first |
| enabled | 2.0.0 | MIT | https://github.com/3rd-Eden/enabled |
| encodeurl | 2.0.0 | MIT | https://github.com/pillarjs/encodeurl |
| engine.io-parser | 5.2.3 | MIT | https://github.com/socketio/socket.io |
| engine.io | 6.6.8 | MIT | https://github.com/socketio/socket.io |
| es-define-property | 1.0.1 | MIT | https://github.com/ljharb/es-define-property |
| es-errors | 1.3.0 | MIT | https://github.com/ljharb/es-errors |
| es-object-atoms | 1.1.1 | MIT | https://github.com/ljharb/es-object-atoms |
| escape-html | 1.0.3 | MIT | https://github.com/component/escape-html |
| etag | 1.8.1 | MIT | https://github.com/jshttp/etag |
| express | 5.2.1 | MIT | https://github.com/expressjs/express |
| fecha | 4.2.3 | MIT | git+https://taylorhakes@github.com/taylorhakes/fecha |
| fetch-blob | 3.2.0 | MIT | https://github.com/node-fetch/fetch-blob |
| finalhandler | 2.1.1 | MIT | https://github.com/pillarjs/finalhandler |
| flatbuffers | 25.9.23 | Apache-2.0 | https://github.com/google/flatbuffers |
| fn.name | 1.1.0 | MIT | https://github.com/3rd-Eden/fn.name |
| formdata-polyfill | 4.0.10 | MIT | git+https://jimmywarting@github.com/jimmywarting/FormData |
| forwarded | 0.2.0 | MIT | https://github.com/jshttp/forwarded |
| fresh | 2.0.0 | MIT | https://github.com/jshttp/fresh |
| function-bind | 1.1.2 | MIT | https://github.com/Raynos/function-bind |
| get-intrinsic | 1.3.0 | MIT | https://github.com/ljharb/get-intrinsic |
| get-proto | 1.0.1 | MIT | https://github.com/ljharb/get-proto |
| gopd | 1.2.0 | MIT | https://github.com/ljharb/gopd |
| h264-profile-level-id | 2.3.2 | ISC | https://github.com/versatica/h264-profile-level-id |
| has-symbols | 1.1.0 | MIT | https://github.com/inspect-js/has-symbols |
| hasown | 2.0.2 | MIT | https://github.com/inspect-js/hasOwn |
| http-errors | 2.0.1 | MIT | https://github.com/jshttp/http-errors |
| iconv-lite | 0.7.2 | MIT | https://github.com/pillarjs/iconv-lite |
| inherits | 2.0.4 | ISC | https://github.com/isaacs/inherits |
| ipaddr.js | 1.9.1 | MIT | https://github.com/whitequark/ipaddr.js |
| is-promise | 4.0.0 | MIT | https://github.com/then/is-promise |
| is-stream | 2.0.1 | MIT | https://github.com/sindresorhus/is-stream |
| jsonwebtoken | 9.0.3 | MIT | https://github.com/auth0/node-jsonwebtoken |
| jwa | 2.0.1 | MIT | https://github.com/brianloveswords/node-jwa |
| jws | 4.0.1 | MIT | https://github.com/brianloveswords/node-jws |
| kuler | 2.0.0 | MIT | https://github.com/3rd-Eden/kuler |
| lodash.includes | 4.3.0 | MIT | https://github.com/lodash/lodash |
| lodash.isboolean | 3.0.3 | MIT | https://github.com/lodash/lodash |
| lodash.isinteger | 4.0.4 | MIT | https://github.com/lodash/lodash |
| lodash.isnumber | 3.0.3 | MIT | https://github.com/lodash/lodash |
| lodash.isplainobject | 4.0.6 | MIT | https://github.com/lodash/lodash |
| lodash.isstring | 4.0.1 | MIT | https://github.com/lodash/lodash |
| lodash.once | 4.1.1 | MIT | https://github.com/lodash/lodash |
| logform | 2.7.0 | MIT | https://github.com/winstonjs/logform |
| math-intrinsics | 1.1.0 | MIT | https://github.com/es-shims/math-intrinsics |
| media-typer | 1.1.0 | MIT | https://github.com/jshttp/media-typer |
| mediasoup | 3.20.0 | ISC | https://github.com/versatica/mediasoup |
| merge-descriptors | 2.0.0 | MIT | https://github.com/sindresorhus/merge-descriptors |
| mime-db | 1.52.0 | MIT | https://github.com/jshttp/mime-db |
| mime-db | 1.54.0 | MIT | https://github.com/jshttp/mime-db |
| mime-types | 2.1.35 | MIT | https://github.com/jshttp/mime-types |
| mime-types | 3.0.2 | MIT | https://github.com/jshttp/mime-types |
| minipass | 7.1.3 | BlueOak-1.0.0 | https://github.com/isaacs/minipass |
| minizlib | 3.1.0 | MIT | https://github.com/isaacs/minizlib |
| ms | 2.1.3 | MIT | https://github.com/vercel/ms |
| nats | 2.29.3 | Apache-2.0 | https://github.com/nats-io/nats.node |
| negotiator | 0.6.3 | MIT | https://github.com/jshttp/negotiator |
| negotiator | 1.0.0 | MIT | https://github.com/jshttp/negotiator |
| nkeys.js | 1.1.0 | Apache-2.0 | https://github.com/nats-io/nkeys.js |
| node-fetch | 3.3.2 | MIT | https://github.com/node-fetch/node-fetch |
| object-assign | 4.1.1 | MIT | https://github.com/sindresorhus/object-assign |
| object-inspect | 1.13.4 | MIT | https://github.com/inspect-js/object-inspect |
| on-finished | 2.4.1 | MIT | https://github.com/jshttp/on-finished |
| once | 1.4.0 | ISC | https://github.com/isaacs/once |
| one-time | 1.0.0 | MIT | https://github.com/3rd-Eden/one-time |
| parseurl | 1.3.3 | MIT | https://github.com/pillarjs/parseurl |
| path-to-regexp | 8.4.0 | MIT | https://github.com/pillarjs/path-to-regexp |
| proxy-addr | 2.0.7 | MIT | https://github.com/jshttp/proxy-addr |
| qs | 6.15.2 | BSD-3-Clause | https://github.com/ljharb/qs |
| range-parser | 1.2.1 | MIT | https://github.com/jshttp/range-parser |
| raw-body | 3.0.2 | MIT | https://github.com/stream-utils/raw-body |
| readable-stream | 3.6.2 | MIT | https://github.com/nodejs/readable-stream |
| redis | 5.12.1 | MIT | https://github.com/redis/node-redis |
| router | 2.2.0 | MIT | https://github.com/pillarjs/router |
| safe-buffer | 5.2.1 | MIT | https://github.com/feross/safe-buffer |
| safe-stable-stringify | 2.5.0 | MIT | https://github.com/BridgeAR/safe-stable-stringify |
| safer-buffer | 2.1.2 | MIT | https://github.com/ChALkeR/safer-buffer |
| semver | 7.7.4 | ISC | https://github.com/npm/node-semver |
| send | 1.2.1 | MIT | https://github.com/pillarjs/send |
| serve-static | 2.2.1 | MIT | https://github.com/expressjs/serve-static |
| setprototypeof | 1.2.0 | ISC | https://github.com/wesleytodd/setprototypeof |
| side-channel-list | 1.0.0 | MIT | https://github.com/ljharb/side-channel-list |
| side-channel-map | 1.0.1 | MIT | https://github.com/ljharb/side-channel-map |
| side-channel-weakmap | 1.0.2 | MIT | https://github.com/ljharb/side-channel-weakmap |
| side-channel | 1.1.0 | MIT | https://github.com/ljharb/side-channel |
| socket.io-adapter | 2.5.7 | MIT | https://github.com/socketio/socket.io |
| socket.io-parser | 4.2.6 | MIT | https://github.com/socketio/socket.io |
| socket.io | 4.8.3 | MIT | https://github.com/socketio/socket.io |
| stack-trace | 0.0.10 | MIT | https://github.com/felixge/node-stack-trace |
| statuses | 2.0.2 | MIT | https://github.com/jshttp/statuses |
| string_decoder | 1.3.0 | MIT | https://github.com/nodejs/string_decoder |
| supports-color | 10.2.2 | MIT | https://github.com/chalk/supports-color |
| tar | 7.5.15 | BlueOak-1.0.0 | https://github.com/isaacs/node-tar |
| text-hex | 1.0.0 | MIT | https://github.com/3rd-Eden/text-hex |
| toidentifier | 1.0.1 | MIT | https://github.com/component/toidentifier |
| triple-beam | 1.4.1 | MIT | https://github.com/winstonjs/triple-beam |
| tweetnacl | 1.0.3 | Unlicense | https://github.com/dchest/tweetnacl-js |
| type-is | 2.0.1 | MIT | https://github.com/jshttp/type-is |
| undici-types | 7.24.6 | MIT | https://github.com/nodejs/undici |
| unpipe | 1.0.0 | MIT | https://github.com/stream-utils/unpipe |
| util-deprecate | 1.0.2 | MIT | https://github.com/TooTallNate/util-deprecate |
| vary | 1.1.2 | MIT | https://github.com/jshttp/vary |
| web-streams-polyfill | 3.3.3 | MIT | https://github.com/MattiasBuelens/web-streams-polyfill |
| winston-transport | 4.9.0 | MIT | https://github.com/winstonjs/winston-transport |
| winston | 3.19.0 | MIT | https://github.com/winstonjs/winston |
| wrappy | 1.0.2 | ISC | https://github.com/npm/wrappy |
| ws | 8.20.1 | MIT | https://github.com/websockets/ws |
| yallist | 5.0.0 | BlueOak-1.0.0 | https://github.com/isaacs/yallist |

---

## Build-Time and Development Dependencies

Additional npm and Go packages are used during development, build, test,
and tooling but are **not** included in distributed binaries. These
include build tools, test frameworks, linters, formatters, type checkers,
and similar development utilities. A complete listing is available in the
respective `package.json` (dev dependencies) and `go.mod` (transitively
through dev tools like `golangci-lint`) at the time of build. Notable
build-time-only attributions:

- **lightningcss** (MPL-2.0) — CSS transformer used by Vite/Parcel at
  build time; not modified by Concord Voice; not included in runtime
  bundles.
- **caniuse-lite** (CC-BY-4.0) — browser compatibility data used by build
  tools; attribution acknowledged.
- **spdx-exceptions** (CC-BY-3.0) — SPDX standard license exceptions
  metadata; attribution acknowledged.
- **HashiCorp libraries** (`errwrap`, `go-cleanhttp`, `go-immutable-radix`,
  `go-multierror`, `go-retryablehttp`, `go-version`, `golang-lru`, `hcl`,
  all MPL-2.0) — used transitively by Go dev tooling; not linked into
  control-plane runtime binary.
- **avast/apkparser** (LGPL-3.0) — Android APK parser used transitively
  by a dev tool; not linked into any shipped binary (confirmed via
  `go mod why github.com/avast/apkparser` returning "main module does
  not need package").

## Regenerating This File

This NOTICE file should be regenerated whenever dependencies are added,
removed, or upgraded. Tooling:

```bash
# Excluded packages: Concord Voice's own packages (private; first-party,
# not third-party) plus the local node-domexception stub override
# documented in [internal]rules/media-plane.md "Docker Build Context Invariant".
EXCLUDES='@concordvoice/desktop;@concordvoice/media-plane;node-domexception-stub'

# Desktop client (npm)
cd client/desktop && npx license-checker --production --csv --excludePackages "$EXCLUDES" > /tmp/desktop-licenses.csv

# Media-plane (npm)
cd services/media-plane && npx license-checker --production --csv --excludePackages "$EXCLUDES" > /tmp/media-licenses.csv

# Go control-plane (binary runtime modules)
cd services/control-plane && go list -deps -e -f '{{if .Module}}{{.Module.Path}} {{.Module.Version}}{{end}}' ./cmd/server | sort -u
```

See [docs/legal/dependency-license-audit.md](docs/legal/dependency-license-audit.md)
for the full audit methodology and license-compatibility framework.

## Contact

For licensing questions about Concord Voice's use of these dependencies,
contact: [contact-us@concordvoice.com](mailto:contact-us@concordvoice.com).

For licensing questions about Concord Voice itself, see [LICENSE](./LICENSE)
and [docs/legal/commercial-license.md](docs/legal/commercial-license.md).
