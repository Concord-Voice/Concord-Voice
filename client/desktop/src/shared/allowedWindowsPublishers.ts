// Single source of truth for the Windows Authenticode publisher allow-list.
//
// Two consumers, one value (#2020):
//  - Build time: scripts/generate-app-update.mts emits this as `publisherName`
//    in the packaged app-update.yml — the ONLY input electron-updater's
//    NsisUpdater.verifySignature reads. Without the key on disk the Windows
//    install-time gate short-circuits to pass and the #644 issuer-pin hook
//    never runs.
//  - Runtime: src/main/updater.ts passes it to setFeedURL (provider config)
//    and the #644 verifyUpdateCodeSignature hook receives it back as the
//    publisherNames argument, populated by electron-updater from the on-disk
//    value.
//
// Array syntax supports multiple allowed publishers (useful during
// cert-rotation transitions across LLC renames). See #404, #644, #2020.
export const ALLOWED_WINDOWS_PUBLISHERS: readonly string[] = Object.freeze(['Concord Voice LLC']);
