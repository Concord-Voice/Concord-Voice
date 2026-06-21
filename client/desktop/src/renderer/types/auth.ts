// Auth configuration types
import type { UserProfile } from '../stores/userStore';
export type { UserProfile } from '../stores/userStore';

export type ConnectionMode = 'hosted' | 'hosted-login' | 'self-hosted';

export interface E2EEKeys {
  wrapped_private_key: string;
  key_derivation_salt: string;
  key_version: number;
}

export interface RegisterRequest {
  email: string;
  username: string;
  password: string;
  age_confirmation: boolean;
  wrapped_private_key: string;
  key_derivation_salt: string;
}

export interface LoginRequest {
  email: string;
  password: string;
  remember_me?: boolean;
}

export interface LoginResponse {
  access_token: string;
  expires_in: number;
  remember_me: boolean;
  user: UserProfile;
  e2ee_keys: E2EEKeys;
}
