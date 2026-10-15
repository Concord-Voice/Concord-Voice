// CLI entry for the packaged-app read-back; the checks live in verify-packaged-app.lib.mts.
//
//   npx tsx scripts/verify-packaged-app.mts fuses       [--out out]
//   npx tsx scripts/verify-packaged-app.mts asar-digest [--out out]    (macOS only)
//
// There is deliberately NO "am I the main module?" guard here. The generator scripts in this
// directory gate their CLI shim on `import.meta.url === process.argv[1]`, which is harmless for a
// generator (a skipped shim leaves an empty file that a later step trips over) and fatal for a
// verifier: when the two paths differ as text (a symlinked directory, drive-letter case, a tsx
// change to argv) the shim is skipped, nothing is checked, and the step exits 0. Keeping the entry
// in a file of its own means there is nothing to compare: importing this file IS running it, and
// the tests import the lib instead. verify-packaged-app.test.ts runs this file for real, directly
// and through a symlink, and requires it to run main().

import { main } from './verify-packaged-app.lib.mts';

process.exitCode = await main(process.argv.slice(2));
