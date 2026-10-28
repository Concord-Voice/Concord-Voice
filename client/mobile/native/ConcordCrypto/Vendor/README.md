# Vendored Argon2 reference implementation

`argon2/` is the Argon2 reference C implementation (P-H-C/phc-winner-argon2), used only
for Argon2id key derivation in the #663 evaluation probe. It is dual-licensed CC0 1.0 or
Apache 2.0 (`argon2/LICENSE`).

- Source: the `argon2/` directory of the `argon2@0.41.1` npm tarball, which bundles the
  reference implementation unchanged. Taken on 2026-10-22.
- Files kept: the portable reference path only. `opt.c` and `blamka-round-opt.h` (x86 SIMD)
  and `thread.c` are left out; the pod builds with `ARGON2_NO_THREADS`, which computes
  the four lanes sequentially and gives the same output.
- Checked: with t=3, m=65536 KiB, p=4 and a 32-byte output, the build here and the desktop's
  `hash-wasm` produced the same hash for the same password and salt. The XCTests check the
  same against the desktop-exported vectors.

SHA-256 of the C sources as vendored:

| File | SHA-256 |
| --- | --- |
| `src/argon2.c` | `72a93deebc5fd76bec0c6d300a2d92b500fb354b26babbddbc6d8b88681e663d` |
| `src/core.c` | `0f53eb2370f8971f04fcf043b8083bf81d1530c48b2ead71c0b6c4e22a01aeec` |
| `src/ref.c` | `4ec47b080c22f4ee416b9dbcfae70ee6007fcfba427f870d7b84395846fa1bc7` |
| `src/encoding.c` | `4d7c05da2cbd5f147bd5bc3bc3fe739a518d09f46c4d122590d6589e1a333dd7` |
| `src/blake2/blake2b.c` | `52519ccbc1e48f489ff444091f630d05dadcc8f1ee61c5b7361e3a9618299c9a` |

`src/encoding.c` differs from the tarball only by one trailing blank line, which the repo's
end-of-file pre-commit hook removes; the tarball's own hash is
`e20124ec0f780f88527e79cf00825e839624ed1f469c7e231596bf070f8c7b14`.

Do not edit these files. Replace them as a set, and record the new source and hashes here.
