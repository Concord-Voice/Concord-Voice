{
  # THE SYNTHETIC TARGET IS OPT-IN, and that is constraint C10 expressed as a
  # build property rather than as a payload allowlist entry.
  #
  # A default build -- which is what `npm run build:native` and therefore the forge
  # packaging step run -- produces ONLY concord_audiocap.node. The synthetic
  # variant is not merely excluded from the archive; on the release path it is
  # never compiled at all, so `EXPECTED_NATIVE` in verify-asar-payload.sh stays at
  # its two entries and the `native/` lookahead in forge.config.ts is untouched.
  # Both of those files remaining unedited is the check that this gate is
  # configured correctly; if a change here starts requiring an edit to either, the
  # gate is wrong, not the allowlist.
  #
  # Build the CI/test variant with:
  #   npx node-gyp rebuild -- -Daudiocap_synthetic=1
  'variables': {
    'audiocap_synthetic%': 0,
  },

  # Shared by both targets, so the release and CI/test binaries cannot drift in
  # anything except the one define that separates them.
  'target_defaults': {
    'sources': [ 'napi/addon.cc' ],
    'include_dirs': [ '.' ],

    # NAPI_VERSION pins the ABI surface. 8 is available in every Node and Electron
    # this project supports and is what keeps a rebuilt .node loadable across
    # Electron upgrades without recompiling.
    'defines': [ 'NAPI_VERSION=8', 'NAPI_DISABLE_CPP_EXCEPTIONS' ],

    # No exceptions and no RTTI anywhere in the addon. AV 208 binds rt/ only, but
    # the seam uses the C node_api.h and has nothing to throw either, so turning
    # them off for the whole target costs nothing and removes the unwinder from a
    # process that hosts an audio callback.
    'cflags_cc':  [ '-std=c++17', '-fno-exceptions', '-fno-rtti' ],
    'cflags_cc!': [ '-fexceptions' ],

    'conditions': [
      [ "OS=='mac'", {
        # The Core Audio backend. The .mm is thin by design -- ten HAL wrappers
        # and the singleton -- because the state machine above it lives in
        # rt/platform/macos/tap_backend.h and is compiled by the LINUX sanitizer
        # legs against a fake HAL. That is the only coverage it can have:
        # client/desktop/native/** is outside sonar.sources.
        'sources': [ 'rt/platform/macos/tap_backend.mm' ],
        'xcode_settings': {
          'CLANG_CXX_LANGUAGE_STANDARD': 'c++17',
          'CLANG_CXX_LIBRARY': 'libc++',
          'GCC_ENABLE_CPP_EXCEPTIONS': 'NO',
          'GCC_ENABLE_CPP_RTTI': 'NO',
          'CLANG_ENABLE_OBJC_ARC': 'YES',
          # DEPLOYMENT TARGET STAYS 11.0. The tap symbols are
          # API_AVAILABLE(macos(14.2)) and weak-linked; the @available guard in
          # tap_backend.mm is what makes that correct, and it sits strictly
          # inside the 14.4 product floor.
          'MACOSX_DEPLOYMENT_TARGET': '11.0',
          # MEASURED JUSTIFICATION, not a style preference: this flag caught a
          # real unguarded availability call while the R9 spike was being built.
          # Without it the omission is a WARNING and the shipped binary
          # null-derefs a weak symbol at runtime on macOS 11-13. This target
          # otherwise sets no warning flags at all. It is a warning flag, not a
          # link flag, so the "no new link flag" property is unaffected.
          'WARNING_CFLAGS': [ '-Werror=unguarded-availability-new' ]
        },
        'link_settings': {
          'libraries': [
            '$(SDKROOT)/System/Library/Frameworks/CoreAudio.framework',
            '$(SDKROOT)/System/Library/Frameworks/Foundation.framework'
          ]
        }
      } ],
      [ "OS=='win'", {
        'msvs_settings': {
          'VCCLCompilerTool': {
            'ExceptionHandling': 0,
            'AdditionalOptions': [ '/std:c++17' ]
          }
        }
      } ]
    ]
  },

  # The SHIPPED addon. It contains no synthetic code at all: with the macro
  # undefined, rt/synthetic_source.h compiles to nothing and start() returns
  # { ok: false, reason: 'NoBackend' }.
  'targets': [
    {
      'target_name': 'concord_audiocap'
    }
  ],

  'conditions': [
    [ 'audiocap_synthetic==1', {
      # CI/TEST ONLY. Same sources, same flags, one extra define. It is a separate
      # .node file with a separate name, and native/concord-audiocap/index.js can
      # only ever resolve `concord_audiocap.node` -- so even a synthetic binary
      # sitting beside the release one in a dev tree is unreachable through the
      # shipped loader.
      'targets': [
        {
          'target_name': 'concord_audiocap_synthetic',
          'defines': [ 'CONCORD_AUDIOCAP_SYNTHETIC=1' ]
        }
      ]
    } ]
  ]
}
