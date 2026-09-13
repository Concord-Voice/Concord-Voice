import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useUserStore } from '../../stores/auth/userStore';
import { useAuthStore } from '../../stores/auth/authStore';
import { useChannelStore } from '../../stores/chat/channelStore';
import { useDMStore } from '../../stores/chat/dmStore';
import { usePermissionStore } from '../../stores/chat/permissionStore';
import {
  updateExpirationPolicy,
  type ExpirationMutationResult,
  type ExpirationPolicy,
  type ExpirationPolicyReadResult,
  type ExpirationRequest,
  type ExpirationScope,
} from '../../services/messaging/expirationPolicyApi';
import {
  captureAuthLifecycle,
  isSameAuthLifecycle,
} from '../../services/system/postLoginHydrationLifecycle';
import { hasPermission, Permissions } from '../../utils/policy/permissions';

export interface ExpirationPolicyControls {
  policy: ExpirationPolicy | null;
  policyState: 'loading' | 'ready' | 'unavailable';
  canEdit: boolean;
  lockedDescription: string;
  onRefresh: () => Promise<ExpirationPolicyReadResult>;
  onApplyPolicy: (request: ExpirationRequest) => Promise<ExpirationMutationResult>;
  onMarkSeen: (revision: number) => void;
  showChangedNotice: boolean;
  onDismissNotice: () => void;
}

type View = {
  scope: ExpirationScope | null;
  serverId?: string;
  accountId?: string;
  authGeneration: number;
  mounted: boolean;
};
type ReadState = { key: string; state: 'loading' | 'ready' | 'unavailable' };

const keyFor = (
  scope: ExpirationScope | null,
  serverId: string | undefined,
  accountId: string | undefined,
  authGeneration: number
) =>
  `${authGeneration}:${accountId ?? ''}:${scope?.kind ?? ''}:${scope?.id ?? ''}:${serverId ?? ''}`;

function currentPolicy(scope: ExpirationScope): ExpirationPolicy | undefined {
  return scope.kind === 'channel'
    ? useChannelStore.getState().channels.find((item) => item.id === scope.id)?.expirationPolicy
    : useDMStore.getState().conversations.find((item) => item.id === scope.id)?.expirationPolicy;
}

function eligible(scope: ExpirationScope, serverId: string | undefined): boolean {
  if (scope.kind === 'channel') {
    const channel = useChannelStore.getState().channels.find((item) => item.id === scope.id);
    return Boolean(serverId) && (!channel || channel.type === 'text');
  }
  return !useDMStore.getState().conversations.find((item) => item.id === scope.id)?.isPersonal;
}

function canEditScope(
  scope: ExpirationScope,
  serverId: string | undefined,
  accountId: string
): boolean {
  if (!eligible(scope, serverId)) return false;
  if (scope.kind === 'channel') {
    const permissions =
      usePermissionStore.getState().channelPermissions[scope.id] ??
      (serverId ? usePermissionStore.getState().serverPermissions[serverId] : undefined);
    return permissions !== undefined && hasPermission(permissions, Permissions.MANAGE_CHANNELS);
  }
  const conversation = useDMStore.getState().conversations.find((item) => item.id === scope.id);
  if (!conversation || conversation.isPersonal) return false;
  const participant = conversation.participants.find((item) => item.userId === accountId);
  return conversation.isGroup ? participant?.role === 'admin' : participant !== undefined;
}

function displayedPolicy(
  inactive: boolean,
  channelPolicy: ExpirationPolicy | undefined,
  conversationPolicy: ExpirationPolicy | undefined
): ExpirationPolicy | null {
  if (inactive) return null;
  return channelPolicy ?? conversationPolicy ?? null;
}

function displayedPolicyState(
  inactive: boolean,
  read: ReadState,
  key: string
): ExpirationPolicyControls['policyState'] {
  if (inactive) return 'unavailable';
  if (read.key === key) return read.state;
  return 'loading';
}

function expirationLockedDescription(
  scope: ExpirationScope | null,
  accountId: string | undefined,
  conversation: ReturnType<typeof useDMStore.getState>['conversations'][number] | undefined
): string {
  if (!scope || !accountId) return 'Message expiration is unavailable.';
  if (scope.kind === 'channel')
    return 'Only members who can manage this channel can change this timer.';
  if (conversation?.isPersonal)
    return 'Message expiration is unavailable for personal conversations.';
  if (conversation?.isGroup) return 'Only group administrators can change this timer.';
  return 'You must be a participant in this conversation to change this timer.';
}

function mutationPolicy(result: ExpirationMutationResult): ExpirationPolicy | undefined {
  if (result.kind === 'ok' || result.kind === 'conflict') return result.policy;
  return undefined;
}

export function useExpirationPolicy(
  scope: ExpirationScope | null,
  serverId?: string
): ExpirationPolicyControls {
  const accountId = useUserStore((state) => state.user?.id);
  const authGeneration = useAuthStore((state) => state.authGeneration);
  const channel = useChannelStore((state) =>
    scope?.kind === 'channel' ? state.channels.find((item) => item.id === scope.id) : undefined
  );
  const conversation = useDMStore((state) =>
    scope?.kind === 'dm' ? state.conversations.find((item) => item.id === scope.id) : undefined
  );
  const channelSeen = useChannelStore((state) =>
    scope?.kind === 'channel' && accountId
      ? state.seenExpirationRevisionsByAccount[accountId]?.[scope.id]
      : undefined
  );
  const dmSeen = useDMStore((state) =>
    scope?.kind === 'dm' && accountId
      ? state.seenExpirationRevisionsByAccount[accountId]?.[scope.id]
      : undefined
  );
  const channelPolicyInvalid = useChannelStore((state) =>
    scope?.kind === 'channel' ? state.invalidExpirationPolicyIds[scope.id] : undefined
  );
  const dmPolicyInvalid = useDMStore((state) =>
    scope?.kind === 'dm' ? state.invalidExpirationPolicyIds[scope.id] : undefined
  );
  const channelPermission = usePermissionStore((state) =>
    scope?.kind === 'channel' ? state.channelPermissions[scope.id] : undefined
  );
  const serverPermission = usePermissionStore((state) =>
    scope?.kind === 'channel' && serverId ? state.serverPermissions[serverId] : undefined
  );
  const scopeKind = scope?.kind;
  const scopeId = scope?.id;
  const key = keyFor(scope, serverId, accountId, authGeneration);
  const [read, setRead] = useState<ReadState>({ key: '', state: 'loading' });
  const operationRef = useRef(0);
  const mutationRef = useRef(0);
  const viewRef = useRef<View>({ scope, serverId, accountId, authGeneration, mounted: false });
  viewRef.current = {
    scope,
    serverId,
    accountId,
    authGeneration,
    mounted: viewRef.current.mounted,
  };

  useEffect(() => {
    viewRef.current.mounted = true;
    return () => {
      viewRef.current.mounted = false;
      operationRef.current += 1;
      mutationRef.current += 1;
    };
  }, []);

  const inactive = !scope || !accountId || !eligible(scope, serverId);
  const policy = displayedPolicy(
    inactive,
    channel?.expirationPolicy,
    conversation?.expirationPolicy
  );
  const policyState = displayedPolicyState(
    inactive || Boolean(channelPolicyInvalid ?? dmPolicyInvalid),
    read,
    key
  );
  const canEdit = useMemo(() => {
    if (inactive || !scope || !accountId) return false;
    if (scope.kind === 'channel') {
      const permissions = channelPermission ?? serverPermission;
      return permissions !== undefined && hasPermission(permissions, Permissions.MANAGE_CHANNELS);
    }
    if (!conversation) return false;
    const participant = conversation.participants.find((item) => item.userId === accountId);
    return conversation.isGroup ? participant?.role === 'admin' : participant !== undefined;
  }, [accountId, channelPermission, conversation, inactive, scope, serverPermission]);
  const lockedDescription = expirationLockedDescription(scope, accountId, conversation);

  const isCurrent = useCallback(
    (captured: View, lifecycle?: ReturnType<typeof captureAuthLifecycle>) => {
      const live = viewRef.current;
      return (
        live.mounted &&
        live.scope?.kind === captured.scope?.kind &&
        live.scope?.id === captured.scope?.id &&
        live.serverId === captured.serverId &&
        live.accountId === captured.accountId &&
        live.authGeneration === captured.authGeneration &&
        useUserStore.getState().user?.id === captured.accountId &&
        useAuthStore.getState().authGeneration === captured.authGeneration &&
        (!lifecycle || isSameAuthLifecycle(lifecycle))
      );
    },
    []
  );

  const onRefresh = useCallback(async (): Promise<ExpirationPolicyReadResult> => {
    const captured: View = {
      scope: scopeKind && scopeId ? { kind: scopeKind, id: scopeId } : null,
      serverId,
      accountId,
      authGeneration,
      mounted: true,
    };
    if (
      !captured.scope ||
      !captured.accountId ||
      !isCurrent(captured) ||
      !eligible(captured.scope, captured.serverId)
    )
      return { kind: 'superseded' };
    const lifecycle = captureAuthLifecycle();
    const operation = ++operationRef.current;
    const capturedKey = keyFor(
      captured.scope,
      captured.serverId,
      captured.accountId,
      captured.authGeneration
    );
    if (isCurrent(captured, lifecycle)) setRead({ key: capturedKey, state: 'loading' });
    const request = { targetId: captured.scope.id, lifecycle };
    let result: ExpirationPolicyReadResult | undefined;
    if (captured.scope.kind === 'channel') {
      if (!captured.serverId) return { kind: 'superseded' };
      result = await useChannelStore.getState().fetchChannels(captured.serverId, request);
    } else {
      result = await useDMStore.getState().fetchConversations(request);
    }
    if (!isCurrent(captured, lifecycle) || operation !== operationRef.current)
      return { kind: 'superseded' };
    const normalized = result ?? { kind: 'unavailable' as const };
    setRead({ key: capturedKey, state: normalized.kind === 'fresh' ? 'ready' : 'unavailable' });
    return normalized;
  }, [accountId, authGeneration, isCurrent, scopeId, scopeKind, serverId]);

  useEffect(() => {
    if (!inactive) void onRefresh();
  }, [inactive, key, onRefresh]);

  useEffect(() => {
    const refreshOnRecovery = () => {
      if (!inactive) void onRefresh();
    };
    globalThis.addEventListener('connection-recovered', refreshOnRecovery);
    return () => globalThis.removeEventListener('connection-recovered', refreshOnRecovery);
  }, [inactive, onRefresh]);

  const onMarkSeen = useCallback(
    (revision: number) => {
      const captured: View = {
        scope: scopeKind && scopeId ? { kind: scopeKind, id: scopeId } : null,
        serverId,
        accountId,
        authGeneration,
        mounted: true,
      };
      if (
        !captured.scope ||
        !captured.accountId ||
        !Number.isSafeInteger(revision) ||
        revision < 0 ||
        !isCurrent(captured) ||
        !eligible(captured.scope, captured.serverId) ||
        currentPolicy(captured.scope)?.revision !== revision
      )
        return;
      if (captured.scope.kind === 'channel')
        useChannelStore
          .getState()
          .markExpirationSeen(captured.accountId, captured.scope.id, revision);
      else
        useDMStore.getState().markExpirationSeen(captured.accountId, captured.scope.id, revision);
    },
    [accountId, authGeneration, isCurrent, scopeId, scopeKind, serverId]
  );

  const seenRevision = scope?.kind === 'channel' ? channelSeen : dmSeen;
  useEffect(() => {
    if (policyState === 'ready' && policy && seenRevision === undefined)
      onMarkSeen(policy.revision);
  }, [onMarkSeen, policy, policyState, seenRevision]);
  const showChangedNotice =
    policyState === 'ready' &&
    policy !== null &&
    seenRevision !== undefined &&
    seenRevision < policy.revision;
  const onDismissNotice = useCallback(() => {
    if (policyState === 'ready' && policy) onMarkSeen(policy.revision);
  }, [onMarkSeen, policy, policyState]);

  const onApplyPolicy = useCallback(
    async (request: ExpirationRequest): Promise<ExpirationMutationResult> => {
      const captured: View = {
        scope: scopeKind && scopeId ? { kind: scopeKind, id: scopeId } : null,
        serverId,
        accountId,
        authGeneration,
        mounted: true,
      };
      if (
        !captured.scope ||
        !captured.accountId ||
        !isCurrent(captured) ||
        !canEditScope(captured.scope, captured.serverId, captured.accountId)
      )
        return { kind: 'rejected', reason: 'unavailable' };
      const lifecycle = captureAuthLifecycle();
      const operation = ++mutationRef.current;
      const result = await updateExpirationPolicy(captured.scope, request);
      if (!isCurrent(captured, lifecycle) || operation !== mutationRef.current)
        return { kind: 'ambiguous' };
      const next = mutationPolicy(result);
      if (next) {
        if (captured.scope.kind === 'channel')
          useChannelStore.getState().applyExpirationPolicy(captured.scope.id, next);
        else useDMStore.getState().applyExpirationPolicy(captured.scope.id, next);
        const capturedKey = keyFor(
          captured.scope,
          captured.serverId,
          captured.accountId,
          captured.authGeneration
        );
        if (
          result.kind === 'ok' &&
          isCurrent(captured, lifecycle) &&
          currentPolicy(captured.scope)?.revision === next.revision
        )
          onMarkSeen(next.revision);
        if (result.kind === 'ok' && isCurrent(captured, lifecycle))
          setRead({ key: capturedKey, state: 'ready' });
      }
      return result;
    },
    [accountId, authGeneration, isCurrent, onMarkSeen, scopeId, scopeKind, serverId]
  );

  return {
    policy,
    policyState,
    canEdit,
    lockedDescription,
    onRefresh,
    onApplyPolicy,
    onMarkSeen,
    showChangedNotice,
    onDismissNotice,
  };
}
