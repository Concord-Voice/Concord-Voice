{
  "targets": [
    {
      "target_name": "concord_audiocap",
      "sources": [ "napi/addon.cc" ],
      "include_dirs": [ "." ],

      # NAPI_VERSION pins the ABI surface. 8 is available in every Node and Electron
      # this project supports and is what keeps a rebuilt .node loadable across
      # Electron upgrades without recompiling.
      "defines": [ "NAPI_VERSION=8", "NAPI_DISABLE_CPP_EXCEPTIONS" ],

      # No exceptions and no RTTI anywhere in the addon. AV 208 binds rt/ only, but
      # the seam uses the C node_api.h and has nothing to throw either, so turning
      # them off for the whole target costs nothing and removes the unwinder from a
      # process that hosts an audio callback.
      "cflags_cc":  [ "-std=c++17", "-fno-exceptions", "-fno-rtti" ],
      "cflags_cc!": [ "-fexceptions" ],

      "conditions": [
        [ "OS=='mac'", {
          "xcode_settings": {
            "CLANG_CXX_LANGUAGE_STANDARD": "c++17",
            "CLANG_CXX_LIBRARY": "libc++",
            "GCC_ENABLE_CPP_EXCEPTIONS": "NO",
            "GCC_ENABLE_CPP_RTTI": "NO",
            "MACOSX_DEPLOYMENT_TARGET": "11.0"
          }
        } ],
        [ "OS=='win'", {
          "msvs_settings": {
            "VCCLCompilerTool": {
              "ExceptionHandling": 0,
              "AdditionalOptions": [ "/std:c++17" ]
            }
          }
        } ]
      ]
    }
  ]
}
