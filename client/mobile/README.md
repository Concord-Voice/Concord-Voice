# Concord Voice mobile

The React Native client for iOS (#206) and Android (#205). It lives here until it
is extracted into its own repository.

## Layout

- `App.tsx`: a placeholder shell. The UI will be built from the internal design prototype.
- `native/ConcordCrypto/`: the native message-crypto pod, which uses CryptoKit
  AES-256-GCM, Security.framework RSA-OAEP-4096, vendored reference Argon2id, and a
  biometry-bound Keychain. Keys stay native, and JS holds only opaque handles. JS can
  still ask for a channel key to be wrapped to any public key it passes, so the JS
  bundle is inside the trust boundary.
- `src/ConcordCrypto.ts`: the typed JS bridge to that module.
- `src/core/`: the control-plane REST client and the DM WebSocket.

The crypto and core code come from the #663 framework probe.

## Run

```sh
npm ci
npm test && npm run typecheck && npm run lint
npm run format:check                                # pinned Prettier 3
npm start                                           # Metro
cd ios && bundle install && bundle exec pod install && cd ..
npm run ios                                         # or: npm run android
```

`bundle install` needs Ruby 3.2 or newer, so install one with Homebrew or rbenv first; macOS's system Ruby 2.6 is too old.

To verify a Jest upgrade, check discovery and execute the suites without a cached
transform:

```sh
npm test -- --listTests --maxWorkers=2
npm test -- --runInBand --no-cache
npm ls jest babel-jest jest-environment-node @react-native/jest-preset
```

The Jest 30 runner resolves its Babel transformer to `babel-jest` 30 while using
the React Native 0.87.1 preset's native environment, which extends
`jest-environment-node` 29. The installed dependency tree also contains Babel
transformer 29 through the preset. Check `npm test -- --showConfig` when changing
these dependencies; the App, core and ConcordCrypto suites exercise rendering,
timers and native-module mocks together. Native crypto execution still requires
the iOS test target.

The repository refuses keystore files, so Android debug builds need a local one:

```sh
keytool -genkeypair -keystore android/app/debug.keystore -storepass android \
  -alias androiddebugkey -keypass android -keyalg RSA -keysize 2048 \
  -validity 10000 -dname "CN=Android Debug,O=Android,C=US"
```

The iOS deployment target is 16.0. iOS 16 is best-effort and untested. The tested
range is iOS 17.5 and later on simulators. Android minSdk is 26.

Release builds are unsigned on purpose until a release key exists. Only Argon2id
accounts can unlock; a legacy key-derivation account is refused with a distinct error.

## Not built yet

- Any real UI.
- MFA login, token refresh and persisted sessions.
- An Android native crypto module.
- CI jobs for this directory.
