import CryptoKit
import Foundation
import Security
import XCTest

@testable import ConcordCrypto

/// Unit tests for the native message crypto, plus the reciprocal desktop-vector tests that run
/// when the harness passes fixture paths (TEST_RUNNER_CONCORD_MESSAGE_VECTORS and
/// TEST_RUNNER_CONCORD_NATIVE_MESSAGE_OUT). The vector tests skip, visibly, without them.
final class ConcordCryptoTests: XCTestCase {
  private static let sharedPair = try? ConcordCrypto.generateRSAKeyPair()

  private func pair() throws -> (privateKey: SecKey, publicKey: SecKey) {
    try XCTUnwrap(Self.sharedPair)
  }

  // MARK: Argon2id

  func testArgon2idMatchesDesktopHashWasmOutput() throws {
    // Synthetic input; expected bytes produced by the desktop's hash-wasm with the same parameters.
    let key = try ConcordCrypto.deriveArgon2id(
      password: "synthetic-probe-password-663", salt: Data(repeating: 7, count: 16))
    let hex = key.withUnsafeBytes { $0.map { String(format: "%02x", $0) }.joined() }
    XCTAssertEqual(hex, "2f2b6536a5a30171a5b780e7fc40122f7d8485865c38d9e6146f30e06afe892c")  // pragma: allowlist secret (synthetic test value)
  }

  func testArgon2idRejectsShortSalt() {
    XCTAssertThrowsError(
      try ConcordCrypto.deriveArgon2id(password: "p", salt: Data(count: 7))
    ) { XCTAssertEqual($0 as? ConcordCryptoError, .invalidInput) }
  }

  // MARK: AES-256-GCM framing

  func testSealUsesDesktopFramingAndRoundTrips() throws {
    let key = SymmetricKey(size: .bits256)
    let blob = try ConcordCrypto.seal(Data("hello".utf8), key: key)
    XCTAssertEqual(blob.count, 12 + 5 + 16)
    XCTAssertEqual(try ConcordCrypto.open(blob, key: key), Data("hello".utf8))
    XCTAssertNotEqual(try ConcordCrypto.seal(Data(), key: key), try ConcordCrypto.seal(Data(), key: key))
  }

  func testOpenRejectsTamperingTruncationAndWrongKey() throws {
    let key = SymmetricKey(size: .bits256)
    let blob = try ConcordCrypto.seal(Data("hello".utf8), key: key)
    for index in [0, 12, blob.count - 1] {
      var tampered = blob
      tampered[index] ^= 1
      XCTAssertThrowsError(try ConcordCrypto.open(tampered, key: key)) {
        XCTAssertEqual($0 as? ConcordCryptoError, .authenticationFailed)
      }
    }
    XCTAssertThrowsError(try ConcordCrypto.open(blob.prefix(27), key: key)) {
      XCTAssertEqual($0 as? ConcordCryptoError, .invalidInput)
    }
    XCTAssertThrowsError(try ConcordCrypto.open(blob, key: SymmetricKey(size: .bits256))) {
      XCTAssertEqual($0 as? ConcordCryptoError, .authenticationFailed)
    }
    XCTAssertThrowsError(try ConcordCrypto.seal(Data(), key: SymmetricKey(size: .bits128))) {
      XCTAssertEqual($0 as? ConcordCryptoError, .invalidInput)
    }
    XCTAssertThrowsError(try ConcordCrypto.open(blob, key: SymmetricKey(size: .bits128))) {
      XCTAssertEqual($0 as? ConcordCryptoError, .invalidInput)
    }
  }

  func testDecryptMessageRejectsInvalidUTF8() throws {
    let key = SymmetricKey(size: .bits256)
    let blob = try ConcordCrypto.seal(Data([0xff, 0xfe]), key: key)
    XCTAssertThrowsError(try ConcordCrypto.decryptMessage(blob, channelKey: key)) {
      XCTAssertEqual($0 as? ConcordCryptoError, .invalidInput)
    }
  }

  // MARK: Key encodings

  func testSpkiAndPkcs8RoundTripWithWebCryptoPrefixes() throws {
    let keys = try pair()
    let spki = try ConcordCrypto.spki(publicKey: keys.publicKey)
    let pkcs8 = try ConcordCrypto.pkcs8(privateKey: keys.privateKey)
    // Prefixes WebCrypto emits for 4096-bit rsaEncryption keys.
    XCTAssertEqual(spki.prefix(19).hexString, "30820222300d06092a864886f70d0101010500")  // pragma: allowlist secret (synthetic test value)
    XCTAssertEqual(pkcs8.prefix(2).hexString, "3082")
    XCTAssertEqual(pkcs8.dropFirst(4).prefix(18).hexString, "020100300d06092a864886f70d0101010500")

    let secret = Data(repeating: 0x42, count: 32)
    let sealed = try ConcordCrypto.rsaEncrypt(secret, publicKey: try ConcordCrypto.publicKey(spki: spki))
    XCTAssertEqual(
      try ConcordCrypto.rsaDecrypt(sealed, privateKey: try ConcordCrypto.privateKey(pkcs8: pkcs8)),
      secret)
  }

  func testKeyParsersRejectMalformedDer() throws {
    let keys = try pair()
    let spki = try ConcordCrypto.spki(publicKey: keys.publicKey)
    let pkcs8 = try ConcordCrypto.pkcs8(privateKey: keys.privateKey)
    var wrongOid = spki
    wrongOid[14] ^= 1
    var nonMinimalLength = Data([0x30, 0x81, 0x01, 0x00])
    nonMinimalLength.append(contentsOf: [0x00])
    let malformed: [Data] = [
      spki + Data([0x00]),  // trailing byte
      spki.prefix(spki.count - 1),  // truncated
      wrongOid,
      nonMinimalLength,
      Data(),
    ]
    for input in malformed {
      XCTAssertThrowsError(try ConcordCrypto.publicKey(spki: input))
    }
    XCTAssertThrowsError(try ConcordCrypto.privateKey(pkcs8: spki))
    XCTAssertThrowsError(try ConcordCrypto.publicKey(spki: pkcs8))
    XCTAssertThrowsError(try ConcordCrypto.privateKey(pkcs8: pkcs8 + Data([0x00])))
  }

  func testRejectsRsaKeysThatAreNot4096Bits() throws {
    let attributes: [CFString: Any] = [
      kSecAttrKeyType: kSecAttrKeyTypeRSA, kSecAttrKeySizeInBits: 2048, kSecAttrIsPermanent: false,
    ]
    let small = try XCTUnwrap(SecKeyCreateRandomKey(attributes as CFDictionary, nil))
    XCTAssertThrowsError(try ConcordCrypto.privateKey(pkcs8: try ConcordCrypto.pkcs8(privateKey: small)))
    // A 2048-bit public key from the server must be refused before any channel key is wrapped to it.
    let smallPublic = try XCTUnwrap(SecKeyCopyPublicKey(small))
    let smallSpki = try ConcordCrypto.spki(publicKey: smallPublic).base64EncodedString()
    let service = ConcordCryptoService()
    defer { service.store.releaseAll() }
    let channel = service.generateChannelKey()
    XCTAssertThrowsError(
      try service.wrapChannelKey(channelKeyHandle: channel, publicKeySpkiBase64: smallSpki)
    ) { XCTAssertEqual($0 as? ConcordCryptoError, .invalidInput) }
  }

  func testChannelKeyRejectsWrongLengthPlaintext() throws {
    let keys = try pair()
    let sealed = try ConcordCrypto.rsaEncrypt(Data(count: 16), publicKey: keys.publicKey)
    XCTAssertThrowsError(try ConcordCrypto.unwrapChannelKey(sealed, privateKey: keys.privateKey)) {
      XCTAssertEqual($0 as? ConcordCryptoError, .invalidInput)
    }
  }

  // MARK: Handle store

  func testServiceFailsClosedOnUnknownAndWrongKindHandles() throws {
    let service = ConcordCryptoService()
    let channel = service.generateChannelKey()
    XCTAssertThrowsError(try service.encryptMessage("x", channelKeyHandle: "missing")) {
      XCTAssertEqual($0 as? ConcordCryptoError, .unknownHandle)
    }
    XCTAssertThrowsError(try service.unwrapChannelKey(wrappedKeyBase64: "AA==", privateKeyHandle: channel)) {
      XCTAssertEqual($0 as? ConcordCryptoError, .wrongKeyKind)
    }
    XCTAssertThrowsError(try service.decryptMessage("not base64!", channelKeyHandle: channel)) {
      XCTAssertEqual($0 as? ConcordCryptoError, .invalidInput)
    }
    let identity = try service.unlockIdentity(
      password: "synthetic-native", saltBase64: salt16, wrappedPrivateKeyBase64: try wrappedIdentity())  // pragma: allowlist secret (synthetic test value)
    let validSpki = try ConcordCrypto.spki(publicKey: try pair().publicKey).base64EncodedString()
    let rsaHandleAsChannelKey: [() throws -> Void] = [
      { _ = try service.encryptMessage("x", channelKeyHandle: identity) },
      { _ = try service.wrapChannelKey(channelKeyHandle: identity, publicKeySpkiBase64: validSpki) },
    ]
    for call in rsaHandleAsChannelKey {
      XCTAssertThrowsError(try call()) { XCTAssertEqual($0 as? ConcordCryptoError, .wrongKeyKind) }
    }
    service.store.release(identity)
    let blob = try service.encryptMessage("round trip", channelKeyHandle: channel)
    XCTAssertEqual(try service.decryptMessage(blob, channelKeyHandle: channel), "round trip")
    service.store.release(channel)
    XCTAssertEqual(service.store.count, 0)
    XCTAssertThrowsError(try service.decryptMessage(blob, channelKeyHandle: channel)) {
      XCTAssertEqual($0 as? ConcordCryptoError, .unknownHandle)
    }
  }

  private let salt16 = Data(repeating: 9, count: 16).base64EncodedString()

  /// The shared pair wrapped under the synthetic password, so handle tests skip RSA generation.
  private func wrappedIdentity() throws -> String {
    let key = try ConcordCrypto.deriveArgon2id(
      password: "synthetic-native", salt: Data(repeating: 9, count: 16))  // pragma: allowlist secret (synthetic test value)
    return try ConcordCrypto.wrapPrivateKey(try pair().privateKey, wrappingKey: key).base64EncodedString()
  }

  func testChannelKeyRoundTripsBetweenTwoIdentities() throws {
    let service = ConcordCryptoService()
    defer { service.store.releaseAll() }
    let sender = try service.createIdentity(password: "synthetic-native")  // pragma: allowlist secret (synthetic test value)
    let recipient = try service.unlockIdentity(
      password: "synthetic-native", saltBase64: salt16, wrappedPrivateKeyBase64: try wrappedIdentity())  // pragma: allowlist secret (synthetic test value)
    let recipientSpki = try ConcordCrypto.spki(publicKey: try pair().publicKey).base64EncodedString()
    let channel = service.generateChannelKey()
    let wrapped = try service.wrapChannelKey(channelKeyHandle: channel, publicKeySpkiBase64: recipientSpki)
    let unwrapped = try service.unwrapChannelKey(wrappedKeyBase64: wrapped, privateKeyHandle: recipient)
    let blob = try service.encryptMessage("across handles", channelKeyHandle: channel)
    XCTAssertEqual(try service.decryptMessage(blob, channelKeyHandle: unwrapped), "across handles")
    // The sender's own identity cannot open a key wrapped to someone else.
    let senderHandle = try XCTUnwrap(sender["privateKeyHandle"])
    XCTAssertThrowsError(try service.unwrapChannelKey(wrappedKeyBase64: wrapped, privateKeyHandle: senderHandle))
  }

  func testCreateIdentityUnlocksWithItsOwnPasswordOnly() throws {
    let service = ConcordCryptoService()
    let identity = try service.createIdentity(password: "synthetic-native")  // pragma: allowlist secret (synthetic test value)
    let salt = try XCTUnwrap(identity["keyDerivationSalt"])
    let wrapped = try XCTUnwrap(identity["wrappedPrivateKeyBase64"])
    XCTAssertEqual(identity["keyDerivationAlg"], "argon2id")
    XCTAssertNoThrow(
      try service.unlockIdentity(password: "synthetic-native", saltBase64: salt, wrappedPrivateKeyBase64: wrapped)) // pragma: allowlist secret
    XCTAssertThrowsError(
      try service.unlockIdentity(password: "synthetic-other", saltBase64: salt, wrappedPrivateKeyBase64: wrapped)  // pragma: allowlist secret (synthetic test value)
    ) { XCTAssertEqual($0 as? ConcordCryptoError, .authenticationFailed) }
    var tampered = try XCTUnwrap(Data(base64Encoded: wrapped))
    tampered[20] ^= 1
    XCTAssertThrowsError(
      try service.unlockIdentity(
        password: "synthetic-native", saltBase64: salt, wrappedPrivateKeyBase64: tampered.base64EncodedString())  // pragma: allowlist secret (synthetic test value)
    ) { XCTAssertEqual($0 as? ConcordCryptoError, .authenticationFailed) }
    // The created identity and the one unlock that succeeded.
    XCTAssertEqual(service.store.count, 2)
    let created = try XCTUnwrap(identity["privateKeyHandle"])
    service.store.releaseAll()
    XCTAssertEqual(service.store.count, 0)
    XCTAssertThrowsError(try service.unwrapChannelKey(wrappedKeyBase64: "AA==", privateKeyHandle: created)) {
      XCTAssertEqual($0 as? ConcordCryptoError, .unknownHandle)
    }
  }

  // MARK: Reciprocal desktop vectors

  private struct DesktopVectors: Decodable {
    struct Identity: Decodable {
      let password, wrongPassword, saltBase64, wrappedPrivateKeyBase64, publicKeySpkiBase64: String
    }
    struct Message: Decodable { let caseId, plaintext, ciphertextBase64, expectedResult: String }
    struct Argon2Case: Decodable { let caseId, password, saltBase64, hashBase64: String }
    let protocolName: String
    let identity: Identity
    let wrappedChannelKeyBase64: String
    let messages: [Message]
    let argon2: [Argon2Case]

    enum CodingKeys: String, CodingKey {
      case protocolName = "protocol", identity, wrappedChannelKeyBase64, messages, argon2
    }
  }

  private func desktopVectors() throws -> DesktopVectors {
    guard let path = ProcessInfo.processInfo.environment["CONCORD_MESSAGE_VECTORS"] else {
      throw XCTSkip("CONCORD_MESSAGE_VECTORS not set; desktop vector run not requested")
    }
    let vectors = try JSONDecoder().decode(
      DesktopVectors.self, from: try Data(contentsOf: URL(fileURLWithPath: path)))
    XCTAssertEqual(vectors.protocolName, "concord-message-v1")
    return vectors
  }

  func testDesktopVectorsUnlockDecryptAndReject() throws {
    let vectors = try desktopVectors()
    let service = ConcordCryptoService()
    defer { service.store.releaseAll() }
    let identity = try service.unlockIdentity(
      password: vectors.identity.password, saltBase64: vectors.identity.saltBase64,
      wrappedPrivateKeyBase64: vectors.identity.wrappedPrivateKeyBase64)
    XCTAssertThrowsError(
      try service.unlockIdentity(
        password: vectors.identity.wrongPassword, saltBase64: vectors.identity.saltBase64,
        wrappedPrivateKeyBase64: vectors.identity.wrappedPrivateKeyBase64))

    let channel = try service.unwrapChannelKey(
      wrappedKeyBase64: vectors.wrappedChannelKeyBase64, privateKeyHandle: identity)
    // The fixture must carry exactly the matrix asserted below, with these payloads, so a short or
    // mislabelled export cannot pass as full coverage.
    let expectedOk: [String: String] = [
      "empty": "", "ascii": "Synthetic probe message.", "unicode": "Grüße 👋🏽 — ñ, 漢字, é",
      "long": String(repeating: "x", count: 65_536),
    ]
    let expectedReject: Set<String> = [
      "modified-iv", "modified-ciphertext", "modified-tag", "truncated-below-minimum",
      "wrong-channel-key",
    ]
    XCTAssertEqual(
      vectors.messages.map(\.caseId).sorted(), (Array(expectedOk.keys) + Array(expectedReject)).sorted())
    for message in vectors.messages {
      let expected = expectedOk[message.caseId] == nil ? "reject" : "ok"
      XCTAssertEqual(message.expectedResult, expected, message.caseId)
      if let plaintext = expectedOk[message.caseId] {
        XCTAssertEqual(message.plaintext, plaintext, message.caseId)
      }
    }
    XCTAssertEqual(vectors.argon2.map(\.caseId).sorted(), ["ascii", "unicode"])
    var positives = 0
    var negatives = 0
    for message in vectors.messages {
      if message.expectedResult == "ok" {
        XCTAssertEqual(
          try service.decryptMessage(message.ciphertextBase64, channelKeyHandle: channel),
          message.plaintext, message.caseId)
        positives += 1
      } else {
        XCTAssertThrowsError(
          try service.decryptMessage(message.ciphertextBase64, channelKeyHandle: channel),
          message.caseId)
        negatives += 1
      }
    }
    XCTAssertGreaterThan(positives, 0)
    XCTAssertGreaterThan(negatives, 0)

    for vector in vectors.argon2 {
      let key = try ConcordCrypto.deriveArgon2id(
        password: vector.password, salt: try XCTUnwrap(Data(base64Encoded: vector.saltBase64)))
      XCTAssertEqual(key.withUnsafeBytes { Data($0) }.base64EncodedString(), vector.hashBase64, vector.caseId)
    }
  }

  func testWritesNativeVectorsForTheDesktopVerifier() throws {
    let vectors = try desktopVectors()
    guard let outPath = ProcessInfo.processInfo.environment["CONCORD_NATIVE_MESSAGE_OUT"] else {
      throw XCTSkip("CONCORD_NATIVE_MESSAGE_OUT not set; native vector export not requested")
    }
    // The export carries a synthetic password and key; only the probe workspace may receive it.
    let parent = URL(fileURLWithPath: outPath).deletingLastPathComponent().resolvingSymlinksInPath().path
    guard parent.hasSuffix("/.superpowers/sdd/2026-10-13-663-native-probes") else {
      return XCTFail("CONCORD_NATIVE_MESSAGE_OUT must sit in the 663 native-probes workspace")
    }
    let service = ConcordCryptoService()
    defer { service.store.releaseAll() }
    let password = "synthetic-native-probe-password-663"  // pragma: allowlist secret (synthetic test value)
    let identity = try service.createIdentity(password: password)
    let channel = service.generateChannelKey()
    let texts: [(String, String)] = [
      ("empty", ""), ("ascii", "Synthetic native message."),
      ("unicode", "Grüße 👋🏽 — ñ, 漢字, e\u{301}"), ("long", String(repeating: "y", count: 65_536)),
    ]
    let output: [String: Any] = [
      "schemaVersion": 1,
      "classification": "synthetic-only",
      "protocol": "concord-message-v1",
      "direction": "native-to-desktop",
      "identity": [
        "password": password,
        "keyDerivationAlg": "argon2id",
        "saltBase64": try XCTUnwrap(identity["keyDerivationSalt"]),
        "wrappedPrivateKeyBase64": try XCTUnwrap(identity["wrappedPrivateKeyBase64"]),
        "publicKeySpkiBase64": try XCTUnwrap(identity["publicKeySpkiBase64"]),
      ],
      "wrappedChannelKeyBase64": try service.wrapChannelKey(
        channelKeyHandle: channel, publicKeySpkiBase64: try XCTUnwrap(identity["publicKeySpkiBase64"])),
      "peerWrappedChannelKeyBase64": try service.wrapChannelKey(
        channelKeyHandle: channel, publicKeySpkiBase64: vectors.identity.publicKeySpkiBase64),
      "messages": try texts.map { caseId, text in
        ["caseId": caseId, "plaintext": text,
         "ciphertextBase64": try service.encryptMessage(text, channelKeyHandle: channel)]
      },
    ]
    let data = try JSONSerialization.data(withJSONObject: output, options: [.sortedKeys])
    // Create once at mode 0600; never overwrite or follow a symlink.
    let fd = Darwin.open(outPath, O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW, 0o600)
    XCTAssertGreaterThanOrEqual(fd, 0, "cannot create native vector output")
    guard fd >= 0 else { return }
    defer { Darwin.close(fd) }
    let written = data.withUnsafeBytes { Darwin.write(fd, $0.baseAddress, $0.count) }
    XCTAssertEqual(written, data.count)
  }

  // MARK: Keychain status mapping (the Keychain itself runs in the simulator probe)

  func testKeychainStatusesMapToStableErrors() {
    XCTAssertEqual(ConcordKeychain.error(errSecItemNotFound), .notFound)
    XCTAssertEqual(ConcordKeychain.error(errSecUserCanceled), .userCancelled)
    XCTAssertEqual(ConcordKeychain.error(errSecInteractionNotAllowed), .interactionNotAllowed)
    XCTAssertEqual(ConcordKeychain.error(errSecAuthFailed), .authenticationFailed)
    XCTAssertEqual(ConcordKeychain.error(errSecMissingEntitlement), .keychainFailure)
  }
}

private extension Data {
  var hexString: String { map { String(format: "%02x", $0) }.joined() }
}
