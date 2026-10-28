import Foundation
import React

/// React Native bridge for ConcordCryptoService. Errors reject with a stable code only, never
/// with key material, plaintext or an underlying system message.
@objc(ConcordCryptoModule)
final class ConcordCryptoModule: NSObject {
  private let service = ConcordCryptoService()
  // Argon2id holds 64 MiB for a few hundred milliseconds; keep it off the JS and main threads.
  private let queue = DispatchQueue(label: "com.concordvoice.mobile.crypto")

  @objc static func requiresMainQueueSetup() -> Bool { false }

  private func run(
    _ resolve: @escaping RCTPromiseResolveBlock, _ reject: @escaping RCTPromiseRejectBlock,
    _ body: @escaping () throws -> Any
  ) {
    queue.async {
      do {
        resolve(try body())
      } catch let error as ConcordCryptoError {
        reject("E_CONCORD_CRYPTO_\(String(describing: error).uppercased())", "Crypto operation failed", nil)
      } catch {
        reject("E_CONCORD_CRYPTO", "Crypto operation failed", nil)
      }
    }
  }

  @objc func unlockIdentity(
    _ password: String, saltBase64: String, wrappedPrivateKeyBase64: String,
    resolve: @escaping RCTPromiseResolveBlock, reject: @escaping RCTPromiseRejectBlock
  ) {
    run(resolve, reject) {
      try self.service.unlockIdentity(
        password: password, saltBase64: saltBase64,
        wrappedPrivateKeyBase64: wrappedPrivateKeyBase64)
    }
  }

  @objc func createIdentity(
    _ password: String, resolve: @escaping RCTPromiseResolveBlock,
    reject: @escaping RCTPromiseRejectBlock
  ) {
    run(resolve, reject) { try self.service.createIdentity(password: password) }
  }

  @objc func generateChannelKey(
    _ resolve: @escaping RCTPromiseResolveBlock, reject: @escaping RCTPromiseRejectBlock
  ) {
    run(resolve, reject) { self.service.generateChannelKey() }
  }

  @objc func unwrapChannelKey(
    _ wrappedKeyBase64: String, privateKeyHandle: String,
    resolve: @escaping RCTPromiseResolveBlock, reject: @escaping RCTPromiseRejectBlock
  ) {
    run(resolve, reject) {
      try self.service.unwrapChannelKey(
        wrappedKeyBase64: wrappedKeyBase64, privateKeyHandle: privateKeyHandle)
    }
  }

  @objc func wrapChannelKey(
    _ channelKeyHandle: String, publicKeySpkiBase64: String,
    resolve: @escaping RCTPromiseResolveBlock, reject: @escaping RCTPromiseRejectBlock
  ) {
    run(resolve, reject) {
      try self.service.wrapChannelKey(
        channelKeyHandle: channelKeyHandle, publicKeySpkiBase64: publicKeySpkiBase64)
    }
  }

  @objc func encryptMessage(
    _ plaintext: String, channelKeyHandle: String,
    resolve: @escaping RCTPromiseResolveBlock, reject: @escaping RCTPromiseRejectBlock
  ) {
    run(resolve, reject) {
      try self.service.encryptMessage(plaintext, channelKeyHandle: channelKeyHandle)
    }
  }

  @objc func decryptMessage(
    _ ciphertextBase64: String, channelKeyHandle: String,
    resolve: @escaping RCTPromiseResolveBlock, reject: @escaping RCTPromiseRejectBlock
  ) {
    run(resolve, reject) {
      try self.service.decryptMessage(ciphertextBase64, channelKeyHandle: channelKeyHandle)
    }
  }

  @objc func releaseKey(
    _ handle: String, resolve: @escaping RCTPromiseResolveBlock,
    reject: @escaping RCTPromiseRejectBlock
  ) {
    run(resolve, reject) {
      self.service.store.release(handle)
      return NSNull()
    }
  }

  @objc func releaseAll(
    _ resolve: @escaping RCTPromiseResolveBlock, reject: @escaping RCTPromiseRejectBlock
  ) {
    run(resolve, reject) {
      self.service.store.releaseAll()
      return NSNull()
    }
  }

  @objc func persistIdentity(
    _ privateKeyHandle: String, account: String,
    resolve: @escaping RCTPromiseResolveBlock, reject: @escaping RCTPromiseRejectBlock
  ) {
    run(resolve, reject) {
      try self.service.persistIdentity(privateKeyHandle: privateKeyHandle, account: account)
      return NSNull()
    }
  }

  // Runs on the crypto queue: SecItemCopyMatching blocks while the Face ID sheet is up.
  @objc func loadIdentity(
    _ account: String, reason: String, allowUI: Bool,
    resolve: @escaping RCTPromiseResolveBlock, reject: @escaping RCTPromiseRejectBlock
  ) {
    run(resolve, reject) {
      try self.service.loadIdentity(account: account, reason: reason, allowUI: allowUI)
    }
  }

  @objc func deleteIdentity(
    _ account: String, resolve: @escaping RCTPromiseResolveBlock,
    reject: @escaping RCTPromiseRejectBlock
  ) {
    run(resolve, reject) {
      try self.service.keychain.delete(account: account)
      return NSNull()
    }
  }
}
