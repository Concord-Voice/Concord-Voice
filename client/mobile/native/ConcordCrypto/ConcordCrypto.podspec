# Native message crypto for the Concord mobile client (#206), carried over from the #663 probe.
# Core: Swift CryptoKit (AES-256-GCM), Security.framework (RSA-OAEP-4096/SHA-256) and the
# vendored Argon2 reference C (Vendor/argon2, see Vendor/README.md). Keys never reach JS.
Pod::Spec.new do |s|
  s.name         = 'ConcordCrypto'
  s.version      = '0.0.1'
  s.summary      = 'Concord desktop-compatible message crypto for the mobile client.'
  s.homepage     = 'https://github.com/Concord-Voice/Concord-Voice'
  s.license      = { :type => 'Proprietary', :text => 'Concord Voice Source License 1.0' }
  s.author       = 'Concord Voice LLC'
  s.platforms    = { :ios => '16.0' }
  s.source       = { :path => '.' }
  s.swift_version = '5.9'
  s.default_subspecs = 'ReactNative'

  s.subspec 'Core' do |core|
    core.source_files = 'Sources/ConcordCrypto.swift', 'Sources/ConcordKeyStore.swift',
                        'Sources/ConcordKeychain.swift',
                        'Vendor/argon2/include/argon2.h', 'Vendor/argon2/src/**/*.{c,h}'
    core.public_header_files = 'Vendor/argon2/include/argon2.h'
    core.private_header_files = 'Vendor/argon2/src/**/*.h'
    core.frameworks = 'CryptoKit', 'Security', 'LocalAuthentication'
    core.pod_target_xcconfig = {
      'DEFINES_MODULE' => 'YES',
      # Lanes are still 4 (the derived key depends on them); they are computed on one thread.
      'GCC_PREPROCESSOR_DEFINITIONS' => '$(inherited) ARGON2_NO_THREADS=1',
      'HEADER_SEARCH_PATHS' => '$(inherited) "$(PODS_TARGET_SRCROOT)/Vendor/argon2/include"',
    }
  end

  s.subspec 'ReactNative' do |rn|
    rn.dependency 'ConcordCrypto/Core'
    rn.dependency 'React-Core'
    rn.source_files = 'Sources/ConcordCryptoModule.{swift,m}'
  end

  s.test_spec 'Tests' do |test|
    test.requires_app_host = false
    test.dependency 'ConcordCrypto/Core'
    # The test bundle links the whole pod (-ObjC pulls in the RN module), and the prebuilt
    # React.framework loads hermesvm; without this the hostless bundle fails to dlopen.
    test.dependency 'hermes-engine'
    test.source_files = 'Tests/**/*.swift'
  end
end
