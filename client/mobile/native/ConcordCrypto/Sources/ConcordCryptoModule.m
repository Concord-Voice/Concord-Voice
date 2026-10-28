#import <React/RCTBridgeModule.h>

@interface RCT_EXTERN_MODULE(ConcordCryptoModule, NSObject)

RCT_EXTERN_METHOD(unlockIdentity:(NSString *)password
                  saltBase64:(NSString *)saltBase64
                  wrappedPrivateKeyBase64:(NSString *)wrappedPrivateKeyBase64
                  resolve:(RCTPromiseResolveBlock)resolve
                  reject:(RCTPromiseRejectBlock)reject)
RCT_EXTERN_METHOD(createIdentity:(NSString *)password
                  resolve:(RCTPromiseResolveBlock)resolve
                  reject:(RCTPromiseRejectBlock)reject)
RCT_EXTERN_METHOD(generateChannelKey:(RCTPromiseResolveBlock)resolve
                  reject:(RCTPromiseRejectBlock)reject)
RCT_EXTERN_METHOD(unwrapChannelKey:(NSString *)wrappedKeyBase64
                  privateKeyHandle:(NSString *)privateKeyHandle
                  resolve:(RCTPromiseResolveBlock)resolve
                  reject:(RCTPromiseRejectBlock)reject)
RCT_EXTERN_METHOD(wrapChannelKey:(NSString *)channelKeyHandle
                  publicKeySpkiBase64:(NSString *)publicKeySpkiBase64
                  resolve:(RCTPromiseResolveBlock)resolve
                  reject:(RCTPromiseRejectBlock)reject)
RCT_EXTERN_METHOD(encryptMessage:(NSString *)plaintext
                  channelKeyHandle:(NSString *)channelKeyHandle
                  resolve:(RCTPromiseResolveBlock)resolve
                  reject:(RCTPromiseRejectBlock)reject)
RCT_EXTERN_METHOD(decryptMessage:(NSString *)ciphertextBase64
                  channelKeyHandle:(NSString *)channelKeyHandle
                  resolve:(RCTPromiseResolveBlock)resolve
                  reject:(RCTPromiseRejectBlock)reject)
RCT_EXTERN_METHOD(releaseKey:(NSString *)handle
                  resolve:(RCTPromiseResolveBlock)resolve
                  reject:(RCTPromiseRejectBlock)reject)
RCT_EXTERN_METHOD(releaseAll:(RCTPromiseResolveBlock)resolve
                  reject:(RCTPromiseRejectBlock)reject)
RCT_EXTERN_METHOD(persistIdentity:(NSString *)privateKeyHandle
                  account:(NSString *)account
                  resolve:(RCTPromiseResolveBlock)resolve
                  reject:(RCTPromiseRejectBlock)reject)
RCT_EXTERN_METHOD(loadIdentity:(NSString *)account
                  reason:(NSString *)reason
                  allowUI:(BOOL)allowUI
                  resolve:(RCTPromiseResolveBlock)resolve
                  reject:(RCTPromiseRejectBlock)reject)
RCT_EXTERN_METHOD(deleteIdentity:(NSString *)account
                  resolve:(RCTPromiseResolveBlock)resolve
                  reject:(RCTPromiseRejectBlock)reject)

@end
