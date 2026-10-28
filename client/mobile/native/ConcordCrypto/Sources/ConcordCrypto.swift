import CryptoKit
import Foundation
import Security

public enum ConcordCryptoError: Error, Equatable {
  case invalidInput
  case keyDerivationFailed
  case authenticationFailed
  case rsaFailure
  case unknownHandle
  case wrongKeyKind
  case notFound
  case userCancelled
  case interactionNotAllowed
  case keychainFailure
}

/// Desktop-compatible message crypto, matching client/desktop/src/renderer/utils/crypto/crypto.ts.
/// Every failure throws; no path returns partial output or falls back to another algorithm.
public enum ConcordCrypto {
  static let ivLength = 12
  static let tagLength = 16
  static let keyLength = 32
  static let rsaBits = 4096
  // Argon2id parameters, the same as crypto.ts deriveKeyArgon2id and the server's DefaultParams.
  static let argon2Iterations: UInt32 = 3
  static let argon2MemoryKiB: UInt32 = 64 * 1024
  static let argon2Lanes: UInt32 = 4
  static let argon2MinSaltLength = 8
  static let oaep = SecKeyAlgorithm.rsaEncryptionOAEPSHA256

  // MARK: Argon2id

  public static func deriveArgon2id(password: String, salt: Data) throws -> SymmetricKey {
    guard salt.count >= argon2MinSaltLength else { throw ConcordCryptoError.invalidInput }
    var output = [UInt8](repeating: 0, count: keyLength)
    defer { output.withUnsafeMutableBytes { _ = memset_s($0.baseAddress, $0.count, 0, $0.count) } }
    var passwordBytes = Array(password.utf8)
    defer {
      passwordBytes.withUnsafeMutableBytes { _ = memset_s($0.baseAddress, $0.count, 0, $0.count) }
    }
    let status = passwordBytes.withUnsafeBytes { pwd in
      salt.withUnsafeBytes { saltBytes in
        argon2id_hash_raw(
          argon2Iterations, argon2MemoryKiB, argon2Lanes,
          pwd.baseAddress, pwd.count, saltBytes.baseAddress, saltBytes.count,
          &output, output.count)
      }
    }
    guard status == ARGON2_OK.rawValue else { throw ConcordCryptoError.keyDerivationFailed }
    return SymmetricKey(data: output)
  }

  // MARK: AES-256-GCM, framed as 12-byte IV || ciphertext || 16-byte tag

  public static func seal(_ plaintext: Data, key: SymmetricKey) throws -> Data {
    guard key.bitCount == keyLength * 8 else { throw ConcordCryptoError.invalidInput }
    let box = try AES.GCM.seal(plaintext, using: key, nonce: AES.GCM.Nonce())
    guard let combined = box.combined, combined.count == ivLength + plaintext.count + tagLength
    else { throw ConcordCryptoError.invalidInput }
    return combined
  }

  public static func open(_ blob: Data, key: SymmetricKey) throws -> Data {
    guard key.bitCount == keyLength * 8, blob.count >= ivLength + tagLength else {
      throw ConcordCryptoError.invalidInput
    }
    do {
      return try AES.GCM.open(AES.GCM.SealedBox(combined: blob), using: key)
    } catch {
      throw ConcordCryptoError.authenticationFailed
    }
  }

  // MARK: RSA-OAEP-4096 / SHA-256

  public static func generateRSAKeyPair() throws -> (privateKey: SecKey, publicKey: SecKey) {
    let attributes: [CFString: Any] = [
      kSecAttrKeyType: kSecAttrKeyTypeRSA,
      kSecAttrKeySizeInBits: rsaBits,
      kSecAttrIsPermanent: false,
    ]
    var error: Unmanaged<CFError>?
    guard let privateKey = SecKeyCreateRandomKey(attributes as CFDictionary, &error),
      let publicKey = SecKeyCopyPublicKey(privateKey)
    else { throw ConcordCryptoError.rsaFailure }
    return (privateKey, publicKey)
  }

  public static func rsaEncrypt(_ data: Data, publicKey: SecKey) throws -> Data {
    var error: Unmanaged<CFError>?
    guard SecKeyIsAlgorithmSupported(publicKey, .encrypt, oaep),
      let sealed = SecKeyCreateEncryptedData(publicKey, oaep, data as CFData, &error)
    else { throw ConcordCryptoError.rsaFailure }
    return sealed as Data
  }

  public static func rsaDecrypt(_ data: Data, privateKey: SecKey) throws -> Data {
    var error: Unmanaged<CFError>?
    guard SecKeyIsAlgorithmSupported(privateKey, .decrypt, oaep),
      let opened = SecKeyCreateDecryptedData(privateKey, oaep, data as CFData, &error)
    else { throw ConcordCryptoError.rsaFailure }
    return opened as Data
  }

  // MARK: WebCrypto key encodings (SPKI public, PKCS#8 private) <-> Security.framework PKCS#1

  /// DER AlgorithmIdentifier { rsaEncryption, NULL }, exactly as WebCrypto exports it.
  static let rsaAlgorithmIdentifier = Data([
    0x30, 0x0d, 0x06, 0x09, 0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d, 0x01, 0x01, 0x01, 0x05, 0x00,
  ])

  public static func spki(publicKey: SecKey) throws -> Data {
    let pkcs1 = try externalRepresentation(publicKey)
    return DER.sequence(rsaAlgorithmIdentifier + DER.element(0x03, Data([0x00]) + pkcs1))
  }

  public static func publicKey(spki: Data) throws -> SecKey {
    var reader = DER.Reader(spki)
    var outer = try reader.readSequence()
    try reader.finish()
    guard try outer.readElement(0x30, raw: true) == rsaAlgorithmIdentifier else {
      throw ConcordCryptoError.invalidInput
    }
    let bits = try outer.readElement(0x03)
    try outer.finish()
    guard bits.first == 0x00 else { throw ConcordCryptoError.invalidInput }
    return try rsaKey(bits.dropFirst(), keyClass: kSecAttrKeyClassPublic)
  }

  public static func pkcs8(privateKey: SecKey) throws -> Data {
    var pkcs1 = try externalRepresentation(privateKey)
    defer { pkcs1.resetBytes(in: 0..<pkcs1.count) }
    return DER.sequence(
      DER.element(0x02, Data([0x00])) + rsaAlgorithmIdentifier + DER.element(0x04, pkcs1))
  }

  public static func privateKey(pkcs8: Data) throws -> SecKey {
    var reader = DER.Reader(pkcs8)
    var outer = try reader.readSequence()
    try reader.finish()
    guard try outer.readElement(0x02) == Data([0x00]),
      try outer.readElement(0x30, raw: true) == rsaAlgorithmIdentifier
    else { throw ConcordCryptoError.invalidInput }
    var pkcs1 = try outer.readElement(0x04)
    defer { pkcs1.resetBytes(in: 0..<pkcs1.count) }
    // WebCrypto exports no optional attributes; anything after the key is rejected.
    try outer.finish()
    return try rsaKey(pkcs1, keyClass: kSecAttrKeyClassPrivate)
  }

  private static func externalRepresentation(_ key: SecKey) throws -> Data {
    var error: Unmanaged<CFError>?
    guard let data = SecKeyCopyExternalRepresentation(key, &error) else {
      throw ConcordCryptoError.rsaFailure
    }
    return data as Data
  }

  private static func rsaKey(_ pkcs1: Data, keyClass: CFString) throws -> SecKey {
    let attributes: [CFString: Any] = [
      kSecAttrKeyType: kSecAttrKeyTypeRSA,
      kSecAttrKeyClass: keyClass,
    ]
    var error: Unmanaged<CFError>?
    guard let key = SecKeyCreateWithData(pkcs1 as CFData, attributes as CFDictionary, &error),
      let attributesOut = SecKeyCopyAttributes(key) as? [CFString: Any],
      (attributesOut[kSecAttrKeySizeInBits] as? Int) == rsaBits
    else { throw ConcordCryptoError.invalidInput }
    return key
  }

  // MARK: Desktop flows

  /// crypto.ts unwrapPrivateKey: AES-GCM-open the PKCS#8 blob with the Argon2id key.
  public static func unwrapPrivateKey(_ wrapped: Data, wrappingKey: SymmetricKey) throws -> SecKey {
    var pkcs8Bytes = try open(wrapped, key: wrappingKey)
    defer { pkcs8Bytes.resetBytes(in: 0..<pkcs8Bytes.count) }
    return try privateKey(pkcs8: pkcs8Bytes)
  }

  /// crypto.ts wrapPrivateKey.
  public static func wrapPrivateKey(_ privateKey: SecKey, wrappingKey: SymmetricKey) throws -> Data {
    var pkcs8Bytes = try pkcs8(privateKey: privateKey)
    defer { pkcs8Bytes.resetBytes(in: 0..<pkcs8Bytes.count) }
    return try seal(pkcs8Bytes, key: wrappingKey)
  }

  /// crypto.ts unwrapChannelKey: RSA-OAEP-decrypt 32 raw AES key bytes.
  public static func unwrapChannelKey(_ wrapped: Data, privateKey: SecKey) throws -> SymmetricKey {
    var raw = try rsaDecrypt(wrapped, privateKey: privateKey)
    defer { raw.resetBytes(in: 0..<raw.count) }
    guard raw.count == keyLength else { throw ConcordCryptoError.invalidInput }
    return SymmetricKey(data: raw)
  }

  /// crypto.ts wrapChannelKey.
  public static func wrapChannelKey(_ channelKey: SymmetricKey, publicKey: SecKey) throws -> Data {
    guard channelKey.bitCount == keyLength * 8 else { throw ConcordCryptoError.invalidInput }
    return try channelKey.withUnsafeBytes { try rsaEncrypt(Data($0), publicKey: publicKey) }
  }

  /// crypto.ts encryptMessage / decryptMessage: UTF-8 text in the framed AES-GCM blob.
  public static func encryptMessage(_ plaintext: String, channelKey: SymmetricKey) throws -> Data {
    try seal(Data(plaintext.utf8), key: channelKey)
  }

  public static func decryptMessage(_ blob: Data, channelKey: SymmetricKey) throws -> String {
    let plaintext = try open(blob, key: channelKey)
    // TextDecoder replaces invalid UTF-8; authenticated desktop output is always valid UTF-8,
    // so anything else is rejected rather than silently repaired.
    guard let text = String(data: plaintext, encoding: .utf8) else {
      throw ConcordCryptoError.invalidInput
    }
    return text
  }
}

/// Minimal strict DER for the two fixed WebCrypto key structures. Not a general ASN.1 parser.
enum DER {
  static func length(_ count: Int) -> Data {
    if count < 0x80 { return Data([UInt8(count)]) }
    var bytes: [UInt8] = []
    var value = count
    while value > 0 {
      bytes.insert(UInt8(value & 0xff), at: 0)
      value >>= 8
    }
    return Data([0x80 | UInt8(bytes.count)] + bytes)
  }

  static func element(_ tag: UInt8, _ content: Data) -> Data {
    Data([tag]) + length(content.count) + content
  }

  static func sequence(_ content: Data) -> Data { element(0x30, content) }

  struct Reader {
    private let data: Data
    private var offset: Int

    init(_ data: Data) {
      // Rebased copy so offsets start at 0. Zeroing key bytes in Swift is best-effort: this copy,
      // like the CFData the Security framework keeps, is not wiped.
      self.data = Data(data)
      self.offset = 0
    }

    private mutating func byte() throws -> UInt8 {
      guard offset < data.count else { throw ConcordCryptoError.invalidInput }
      defer { offset += 1 }
      return data[data.startIndex + offset]
    }

    private mutating func readLength() throws -> Int {
      let first = try byte()
      if first < 0x80 { return Int(first) }
      let count = Int(first & 0x7f)
      // Definite, minimal lengths only; three bytes is far above any 4096-bit key.
      guard count >= 1, count <= 3 else { throw ConcordCryptoError.invalidInput }
      var value = 0
      for index in 0..<count {
        let next = try byte()
        if index == 0, next == 0 { throw ConcordCryptoError.invalidInput }
        value = (value << 8) | Int(next)
      }
      guard value >= 0x80 else { throw ConcordCryptoError.invalidInput }
      return value
    }

    /// Returns the element's content, or the whole encoded element when `raw` is set.
    mutating func readElement(_ tag: UInt8, raw: Bool = false) throws -> Data {
      let start = offset
      guard try byte() == tag else { throw ConcordCryptoError.invalidInput }
      let count = try readLength()
      guard count <= data.count - offset else { throw ConcordCryptoError.invalidInput }
      let contentStart = data.startIndex + offset
      offset += count
      return raw
        ? data.subdata(in: (data.startIndex + start)..<(data.startIndex + offset))
        : data.subdata(in: contentStart..<(contentStart + count))
    }

    mutating func readSequence() throws -> Reader {
      Reader(try readElement(0x30))
    }

    func finish() throws {
      guard offset == data.count else { throw ConcordCryptoError.invalidInput }
    }
  }
}
