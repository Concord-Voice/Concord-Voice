/** Exact trusted-device recovery v2 wire contract. Binary values use padded standard base64. */
export interface DeviceRecoveryContext {
  request_id: string;
  protocol_version: 2;
  server_origin: string;
  account_binding: string;
  expires_at: number;
  requester_nonce: string;
  requester_public_key: string;
  recovery_token_jti_hash: string;
}
export interface DeviceRecoveryOffer {
  responder_public_key: string;
  responder_nonce: string;
  transcript_hash: string;
}
export type DeviceRecoveryRequest =
  | (DeviceRecoveryContext & { status: 'pending' })
  | (DeviceRecoveryContext & DeviceRecoveryOffer & { status: 'offered' })
  | (DeviceRecoveryContext &
      DeviceRecoveryOffer & { status: 'approved'; encrypted_payload: string })
  | {
      request_id: string;
      protocol_version: 2;
      status: 'rejected' | 'expired' | 'complete';
      expires_at: number;
    };
export interface DeviceRecoveryCreateBody {
  protocol_version: 2;
  recovery_token: string;
  server_origin: string;
  account_binding: string;
  requester_nonce: string;
  requester_public_key: string;
}
export type DeviceRecoveryRespondBody =
  | (DeviceRecoveryOffer & { action: 'offer'; protocol_version: 2 })
  | { action: 'approve'; protocol_version: 2; transcript_hash: string; encrypted_payload: string }
  | { action: 'reject'; protocol_version: 2 };
export interface DeviceRecoveryCompleteBody {
  protocol_version: 2;
  transcript_hash: string;
}
export interface DeviceRecoveryResponseResult {
  request_id: string;
  protocol_version: 2;
  status: 'offered' | 'approved' | 'rejected' | 'complete';
}
