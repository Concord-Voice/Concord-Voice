import CryptoKit
import Foundation
import Security

/// In-memory key handles. JavaScript only ever holds an opaque handle string; key bytes stay
/// native. Wrong-kind and unknown handles fail closed.
public final class ConcordKeyStore {
  enum Entry {
    case symmetric(SymmetricKey)
    case rsaPrivate(SecKey)
  }

  private var entries: [String: Entry] = [:]
  private let lock = NSLock()

  public init() {}

  func add(_ entry: Entry) -> String {
    let handle = UUID().uuidString
    lock.lock()
    defer { lock.unlock() }
    entries[handle] = entry
    return handle
  }

  func symmetric(_ handle: String) throws -> SymmetricKey {
    guard case .symmetric(let key) = try entry(handle) else {
      throw ConcordCryptoError.wrongKeyKind
    }
    return key
  }

  func rsaPrivate(_ handle: String) throws -> SecKey {
    guard case .rsaPrivate(let key) = try entry(handle) else {
      throw ConcordCryptoError.wrongKeyKind
    }
    return key
  }

  public func release(_ handle: String) {
    lock.lock()
    defer { lock.unlock() }
    entries.removeValue(forKey: handle)
  }

  public func releaseAll() {
    lock.lock()
    defer { lock.unlock() }
    entries.removeAll()
  }

  public var count: Int {
    lock.lock()
    defer { lock.unlock() }
    return entries.count
  }

  private func entry(_ handle: String) throws -> Entry {
    lock.lock()
    defer { lock.unlock() }
    guard let entry = entries[handle] else { throw ConcordCryptoError.unknownHandle }
    return entry
  }
}

/// The operations JavaScript can call, in base64 and handles. Shared by the React Native
/// module and the tests, so the tests exercise the same surface the app uses.
public struct ConcordCryptoService {
  public let store: ConcordKeyStore
  public let keychain: ConcordKeychain

  public init(store: ConcordKeyStore = ConcordKeyStore(), keychain: ConcordKeychain = ConcordKeychain()) {
    self.store = store
    self.keychain = keychain
  }

  /// Login: derive the Argon2id key, unwrap the identity key, keep only the private handle.
  public func unlockIdentity(password: String, saltBase64: String, wrappedPrivateKeyBase64: String)
    throws -> String
  {
    let wrappingKey = try ConcordCrypto.deriveArgon2id(
      password: password, salt: try decode(saltBase64))
    let privateKey = try ConcordCrypto.unwrapPrivateKey(
      try decode(wrappedPrivateKeyBase64), wrappingKey: wrappingKey)
    return store.add(.rsaPrivate(privateKey))
  }

  /// Registration: a new RSA-4096 identity wrapped exactly as crypto.ts generateRegistrationKeys.
  public func createIdentity(password: String) throws -> [String: String] {
    var salt = Data(count: 16)
    let status = salt.withUnsafeMutableBytes { buffer -> OSStatus in
      guard let base = buffer.baseAddress else { return errSecAllocate }
      return SecRandomCopyBytes(kSecRandomDefault, buffer.count, base)
    }
    guard status == errSecSuccess else { throw ConcordCryptoError.keyDerivationFailed }
    let wrappingKey = try ConcordCrypto.deriveArgon2id(password: password, salt: salt)
    let pair = try ConcordCrypto.generateRSAKeyPair()
    // Everything that can throw runs first, so a failure never strands a private key in the store.
    let spki = try ConcordCrypto.spki(publicKey: pair.publicKey)
    let wrapped = try ConcordCrypto.wrapPrivateKey(pair.privateKey, wrappingKey: wrappingKey)
    return [
      "privateKeyHandle": store.add(.rsaPrivate(pair.privateKey)),
      "publicKeySpkiBase64": spki.base64EncodedString(),
      "wrappedPrivateKeyBase64": wrapped.base64EncodedString(),
      "keyDerivationSalt": salt.base64EncodedString(),
      "keyDerivationAlg": "argon2id",
    ]
  }

  /// Face ID / Touch ID persistence of an unlocked identity; see ConcordKeychain.
  public func persistIdentity(privateKeyHandle: String, account: String) throws {
    try keychain.save(try store.rsaPrivate(privateKeyHandle), account: account)
  }

  public func loadIdentity(account: String, reason: String, allowUI: Bool) throws -> String {
    store.add(.rsaPrivate(try keychain.load(account: account, reason: reason, allowUI: allowUI)))
  }

  public func generateChannelKey() -> String {
    store.add(.symmetric(SymmetricKey(size: .bits256)))
  }

  public func unwrapChannelKey(wrappedKeyBase64: String, privateKeyHandle: String) throws -> String {
    let key = try ConcordCrypto.unwrapChannelKey(
      try decode(wrappedKeyBase64), privateKey: try store.rsaPrivate(privateKeyHandle))
    return store.add(.symmetric(key))
  }

  public func wrapChannelKey(channelKeyHandle: String, publicKeySpkiBase64: String) throws -> String {
    let publicKey = try ConcordCrypto.publicKey(spki: try decode(publicKeySpkiBase64))
    return try ConcordCrypto.wrapChannelKey(
      try store.symmetric(channelKeyHandle), publicKey: publicKey
    ).base64EncodedString()
  }

  public func encryptMessage(_ plaintext: String, channelKeyHandle: String) throws -> String {
    try ConcordCrypto.encryptMessage(plaintext, channelKey: try store.symmetric(channelKeyHandle))
      .base64EncodedString()
  }

  public func decryptMessage(_ ciphertextBase64: String, channelKeyHandle: String) throws -> String {
    try ConcordCrypto.decryptMessage(
      try decode(ciphertextBase64), channelKey: try store.symmetric(channelKeyHandle))
  }

  private func decode(_ base64: String) throws -> Data {
    guard let data = Data(base64Encoded: base64) else { throw ConcordCryptoError.invalidInput }
    return data
  }
}
