import Foundation
import LocalAuthentication
import Security

/// Keeps the unlocked identity key in the Keychain behind Face ID / Touch ID, so a returning user
/// skips the Argon2id unlock. The item is device-only and bound to the current biometric set:
/// re-enrolling a face or finger makes it unreadable, and nothing reads it without the user.
public struct ConcordKeychain {
  let service: String

  public init(service: String = "com.concordvoice.identity") {
    self.service = service
  }

  public func save(_ privateKey: SecKey, account: String) throws {
    var pkcs8 = try ConcordCrypto.pkcs8(privateKey: privateKey)
    defer { pkcs8.resetBytes(in: 0..<pkcs8.count) }
    var error: Unmanaged<CFError>?
    guard
      let access = SecAccessControlCreateWithFlags(
        nil, kSecAttrAccessibleWhenUnlockedThisDeviceOnly, .biometryCurrentSet, &error)
    else { throw ConcordCryptoError.keychainFailure }
    try delete(account: account)
    let status = SecItemAdd(
      [
        kSecClass: kSecClassGenericPassword,
        kSecAttrService: service,
        kSecAttrAccount: account,
        kSecAttrAccessControl: access,
        kSecValueData: pkcs8,
      ] as CFDictionary, nil)
    guard status == errSecSuccess else { throw Self.error(status) }
  }

  /// With allowUI false the read must fail rather than prompt; callers use that to prove the
  /// item cannot be read silently.
  public func load(account: String, reason: String, allowUI: Bool = true) throws -> SecKey {
    let context = LAContext()
    context.localizedReason = reason
    context.interactionNotAllowed = !allowUI
    var result: CFTypeRef?
    let status = SecItemCopyMatching(
      [
        kSecClass: kSecClassGenericPassword,
        kSecAttrService: service,
        kSecAttrAccount: account,
        kSecReturnData: true,
        kSecUseAuthenticationContext: context,
      ] as CFDictionary, &result)
    guard status == errSecSuccess, var pkcs8 = result as? Data else { throw Self.error(status) }
    defer { pkcs8.resetBytes(in: 0..<pkcs8.count) }
    return try ConcordCrypto.privateKey(pkcs8: pkcs8)
  }

  public func delete(account: String) throws {
    let status = SecItemDelete(
      [kSecClass: kSecClassGenericPassword, kSecAttrService: service, kSecAttrAccount: account]
        as CFDictionary)
    guard status == errSecSuccess || status == errSecItemNotFound else { throw Self.error(status) }
  }

  static func error(_ status: OSStatus) -> ConcordCryptoError {
    switch status {
    case errSecItemNotFound: return .notFound
    case errSecUserCanceled: return .userCancelled
    case errSecInteractionNotAllowed: return .interactionNotAllowed
    case errSecAuthFailed: return .authenticationFailed
    default: return .keychainFailure
    }
  }
}
