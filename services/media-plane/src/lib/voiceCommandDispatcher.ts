import type { Server, Socket } from 'socket.io';
import type { Participant, Room, RoomManager } from './roomManager.js';
import { handleForceDisconnect, type ForceDisconnectIO } from './forceDisconnect.js';
import type { EmitSecurityEvent } from './securityEvent.js';
import { decodeVoiceCommand, isVersionedVoiceCommand, type VoiceCommand } from './voiceCommand.js';

export type DispatchResult = 'applied' | 'already_complete' | 'target_absent';

interface DispatcherOptions {
  roomManager: RoomManager;
  io: Server;
  nodeBootId: string;
  streamGeneration: string;
  ensureAuthority: () => Promise<void>;
  handleLegacy: (data: Record<string, unknown>) => Promise<void>;
  emitSecurityEvent?: EmitSecurityEvent;
}

interface CapturedTarget {
  participant: NonNullable<ReturnType<RoomManager['getProvisionalParticipant']>>;
  participantRef?: Participant;
  admitted?: Participant;
  room: Room;
  provisional: boolean;
  socket?: Socket;
  socketSession: unknown;
  socketRoomId: unknown;
}

interface ActiveCommand {
  command: VoiceCommand;
  capturedDisconnect: CapturedTarget | undefined;
  identity: string;
  sequence: bigint;
  mutationComplete: boolean;
  result: DispatchResult;
  fanout: Array<() => void>;
  nextFanout: number;
  running?: Promise<DispatchResult>;
}

function commandIdentity(command: VoiceCommand): string {
  return JSON.stringify(
    command,
    Object.keys(command)
      .concat('sessionGeneration', 'nodeBootId', 'socketId', 'credentialEpoch')
      .sort((first, second) => {
        if (first === second) return 0;
        return first < second ? -1 : 1;
      })
  );
}

/** Only the durable iterator advances effects; core copies can join its turn. */
export class VoiceCommandDispatcher {
  private active: ActiveCommand | undefined;
  private completedSequence = 0n;

  constructor(private readonly options: DispatcherOptions) {}

  async applyDurable(command: VoiceCommand): Promise<DispatchResult> {
    const sequence = BigInt(command.sequence);
    if (command.streamGeneration !== this.options.streamGeneration) {
      throw new Error('Voice command generation unavailable');
    }
    if (sequence <= this.completedSequence) {
      await this.options.ensureAuthority();
      return 'already_complete';
    }
    const identity = commandIdentity(command);
    if (this.active && this.active.sequence > this.completedSequence) {
      if (this.active.identity !== identity) {
        throw new Error('Voice command predecessor incomplete');
      }
    } else {
      this.active = {
        command: { ...command, target: command.target ? { ...command.target } : null },
        capturedDisconnect: command.kind === 'disconnect' ? this.capture(command) : undefined,
        identity,
        sequence,
        mutationComplete: false,
        result: 'applied',
        fanout: [],
        nextFanout: 0,
      };
    }
    const active = this.active;
    if (active.running) return active.running;
    const running = this.apply(active);
    active.running = running;
    void running
      .finally(() => {
        if (active.running === running) active.running = undefined;
      })
      .catch(() => undefined);
    return running;
  }

  async handleCompatibility(data: Record<string, unknown>): Promise<void> {
    if (!isVersionedVoiceCommand(data)) {
      await this.options.ensureAuthority();
      await this.options.handleLegacy(data);
      return;
    }
    let command: VoiceCommand;
    try {
      command = decodeVoiceCommand(
        new TextEncoder().encode(JSON.stringify(data)),
        this.options.streamGeneration
      );
    } catch {
      try {
        this.options.emitSecurityEvent?.({
          eventType: 'media_integrity',
          outcome: 'denied',
          severity: 'medium',
          reasonCode: 'media_schema_rejected',
        });
      } catch {
        /* Observers cannot change schema admission. */
      }
      return;
    }
    if (BigInt(command.sequence) <= this.completedSequence) return;
    // A future core copy is already retained in JetStream. It cannot overtake
    // that obligation or start an independently ordered mutation rail.
    if (this.active?.identity === commandIdentity(command) && this.active.running) {
      await this.active.running;
    }
  }

  private async apply(active: ActiveCommand): Promise<DispatchResult> {
    if (!active.mutationComplete) {
      await this.options.ensureAuthority();
      await this.mutate(active);
      active.mutationComplete = true;
    }
    for (; active.nextFanout < active.fanout.length; active.nextFanout++) {
      await this.options.ensureAuthority();
      active.fanout[active.nextFanout]?.();
    }
    await this.options.ensureAuthority();
    this.completedSequence = active.sequence;
    if (this.active === active) this.active = undefined;
    return active.result;
  }

  private capture(command: VoiceCommand): CapturedTarget | undefined {
    const target = command.target;
    const roomId = command.channelId;
    if (!target || !roomId || target.nodeBootId !== this.options.nodeBootId) return undefined;
    const socket = this.options.io.sockets.sockets.get(target.socketId);
    const session = socket?.data.voiceEnforcementSession;
    const room = this.options.roomManager.getRoom(roomId);
    if (room?.roomKind !== command.scope) return undefined;
    const admitted = this.options.roomManager.getParticipant(roomId, command.userId);
    const provisional = this.options.roomManager.getProvisionalParticipant(roomId, command.userId);
    const provisionalRef =
      room.pendingDMParticipants.get(command.userId)?.participant ??
      room.pendingChannelParticipants.get(command.userId);
    for (const [participant, pending] of [
      [admitted, false],
      [provisional, true],
    ] as const) {
      if (
        participant?.socketId === target.socketId &&
        participant.voiceEnforcementSessionGeneration === target.sessionGeneration &&
        participant.credentialEpoch === target.credentialEpoch &&
        this.captureSocketMatches(command, socket, pending)
      ) {
        return {
          participant: Object.freeze({
            socketId: participant.socketId,
            credentialEpoch: participant.credentialEpoch,
            voiceEnforcementSessionGeneration: participant.voiceEnforcementSessionGeneration,
            admissionId: participant.admissionId,
          }),
          participantRef: pending ? provisionalRef : participant,
          admitted: pending ? undefined : admitted,
          provisional: pending,
          socket,
          socketSession: session,
          socketRoomId: socket?.data.roomId,
          room,
        };
      }
    }
    return undefined;
  }

  private captureSocketMatches(
    command: VoiceCommand,
    socket: Socket | undefined,
    pending: boolean
  ): boolean {
    if (!socket) return true;
    const target = command.target;
    if (!target) return false;
    const sessionMatches = this.socketSessionMatches(command, socket);
    if (
      socket.data.userId !== command.userId ||
      socket.data.credentialEpoch !== target.credentialEpoch ||
      (socket.data.voiceEnforcementSession !== undefined && !sessionMatches)
    ) {
      return false;
    }
    return pending
      ? socket.data.roomId === undefined || socket.data.roomId === command.channelId
      : sessionMatches && socket.data.roomId === command.channelId;
  }

  private socketSessionMatches(command: VoiceCommand, socket: Socket): boolean {
    const target = command.target;
    const session = socket.data.voiceEnforcementSession;
    return (
      !!target &&
      session?.sessionGeneration === target.sessionGeneration &&
      session.nodeBootId === target.nodeBootId &&
      session.socketId === target.socketId &&
      session.roomId === command.channelId &&
      session.userId === command.userId &&
      session.roomKind === command.scope &&
      session.credentialEpoch === target.credentialEpoch
    );
  }

  private socketCurrent(command: VoiceCommand, captured: CapturedTarget): boolean {
    const target = command.target;
    const socket = captured.socket;
    const roomId = command.channelId;
    if (
      !socket ||
      !target ||
      !roomId ||
      this.options.io.sockets.sockets.get(target.socketId) !== socket ||
      socket.data.userId !== command.userId ||
      socket.data.credentialEpoch !== target.credentialEpoch
    ) {
      return false;
    }
    const manager = this.options.roomManager;
    if (
      [
        manager.getParticipant(roomId, command.userId),
        manager.getProvisionalParticipant(roomId, command.userId),
      ].some(
        (participant) =>
          participant?.socketId === target.socketId &&
          participant.voiceEnforcementSessionGeneration !== target.sessionGeneration
      )
    )
      return false;
    if (socket.data.voiceEnforcementSession !== undefined) {
      return (
        this.socketSessionMatches(command, socket) &&
        (socket.data.roomId === roomId ||
          (captured.provisional && socket.data.roomId === undefined))
      );
    }
    // A1 has not installed its registration response yet. The frozen candidate
    // identity, socket object and authenticated fields survive its exact removal;
    // a newer generation sharing the socket can never inherit this disconnect.
    return (
      captured.provisional &&
      captured.socketSession === undefined &&
      socket.data.roomId === captured.socketRoomId
    );
  }

  private participantCurrent(command: VoiceCommand, captured: CapturedTarget): boolean {
    return (
      this.options.roomManager.getRoom(command.channelId ?? '') === captured.room &&
      this.options.roomManager.getParticipant(command.channelId ?? '', command.userId) ===
        captured.admitted
    );
  }

  private exactParticipantCurrent(command: VoiceCommand, captured: CapturedTarget): boolean {
    const target = command.target;
    const participant = captured.participantRef;
    return (
      !!target &&
      !!participant &&
      this.options.roomManager.getRoom(command.channelId ?? '') === captured.room &&
      this.options.roomManager.getParticipant(command.channelId ?? '', command.userId) ===
        participant &&
      participant.socketId === target.socketId &&
      participant.credentialEpoch === target.credentialEpoch &&
      participant.voiceEnforcementSessionGeneration === target.sessionGeneration
    );
  }

  private async mutate(active: ActiveCommand): Promise<void> {
    const { command } = active;
    // Exact local removal can precede a failed native proof. Redelivery must
    // finish that captured session's effects rather than recapture its absence.
    const captured =
      command.kind === 'disconnect' ? active.capturedDisconnect : this.capture(command);
    if (!captured || !command.channelId || !command.target) {
      if (command.channelId && command.target?.nodeBootId === this.options.nodeBootId) {
        await this.options.roomManager.awaitNativeTermination(
          command.channelId,
          command.userId,
          command.target.socketId,
          command.target.sessionGeneration
        );
      }
      active.result = 'target_absent';
      return;
    }
    if (command.kind === 'disconnect') {
      await this.disconnect(active, captured);
      return;
    }
    if (command.kind === 'server_moderation') {
      const applied = await this.options.roomManager.applyExactServerEnforcementSnapshot(
        command.channelId,
        command.userId,
        command.target,
        command.serverMuted === true,
        command.serverDeafened === true,
        BigInt(command.authorizationRevision ?? '0')
      );
      if (!applied) {
        active.result = 'target_absent';
        return;
      }
      const event =
        command.action === 'mute' || command.action === 'unmute'
          ? 'server-mute-changed'
          : 'server-deafen-changed';
      const payload = {
        userId: command.userId,
        serverMuted: command.serverMuted,
        serverDeafened: command.serverDeafened,
      };
      active.fanout.push(() => {
        if (
          this.socketCurrent(command, captured) &&
          this.participantCurrent(command, captured) &&
          captured.admitted?.serverEnforcementRevision ===
            BigInt(command.authorizationRevision ?? '0')
        ) {
          this.options.io.to(command.channelId ?? '').emit(event, payload);
        }
      });
      return;
    }
    // A captured candidate may promote during the authority await. The exact
    // Room and Participant identity is checked before every native pause.
    const participantRef = captured.participantRef;
    if (!participantRef) return;
    await this.options.ensureAuthority();
    const exact = {
      room: captured.room,
      participant: participantRef,
      target: command.target,
    };
    if (command.action === 'mute') {
      await this.options.roomManager.userMuteParticipant(command.channelId, command.userId, exact);
    } else {
      await this.options.roomManager.userDeafenParticipant(
        command.channelId,
        command.userId,
        exact
      );
    }
    if (!this.exactParticipantCurrent(command, captured)) return;
    const fanout = [...participantRef.producers]
      .filter(([, entry]) => entry.kind === 'audio')
      .map(([producerId, entry]) => ({
        producerId,
        userId: command.userId,
        kind: entry.kind,
        source: entry.source,
      }));
    active.fanout.push(
      ...fanout.map((payload) => () => {
        if (
          this.socketCurrent(command, captured) &&
          this.exactParticipantCurrent(command, captured)
        ) {
          this.options.io.to(command.channelId ?? '').emit('producer-paused', payload);
        }
      })
    );
  }

  private async disconnect(active: ActiveCommand, captured: CapturedTarget): Promise<void> {
    const { command } = active;
    const roomId = command.channelId;
    if (!roomId) return;
    const manager = this.options.roomManager;
    // Restrict the existing helper to captured objects. Omitting its paired
    // admission fields is safe here, including deployed legacy sessions that
    // have no admissionId: this adapter exposes no broad same-user successor.
    const adapter = {
      getRoom: () => manager.getRoom(roomId),
      awaitNativeTermination: async () => {
        await this.options.ensureAuthority();
        await manager.awaitNativeTermination(
          roomId,
          command.userId,
          captured.participant.socketId,
          captured.participant.voiceEnforcementSessionGeneration
        );
      },
      getParticipant: () => captured.admitted,
      getProvisionalParticipant: () => (captured.provisional ? captured.participant : undefined),
      removeProvisionalParticipantForEnforcement: async () => {
        await this.options.ensureAuthority();
        const admitted = manager.getParticipant(roomId, command.userId);
        const current =
          admitted !== undefined && admitted === captured.participantRef
            ? admitted
            : manager.getProvisionalParticipant(roomId, command.userId);
        if (
          manager.getRoom(roomId) !== captured.room ||
          current?.socketId !== captured.participant.socketId ||
          current.credentialEpoch !== captured.participant.credentialEpoch ||
          current.voiceEnforcementSessionGeneration !==
            captured.participant.voiceEnforcementSessionGeneration
        ) {
          return false;
        }
        if (current === admitted) {
          return manager.leaveRoomIfSocketOwned(
            roomId,
            command.userId,
            captured.participant.socketId,
            captured.participant.voiceEnforcementSessionGeneration
          );
        }
        return manager.removeProvisionalParticipantForEnforcement(
          roomId,
          command.userId,
          captured.participant.socketId,
          captured.participant.voiceEnforcementSessionGeneration
        );
      },
      leaveRoomIfSocketOwned: async () => {
        await this.options.ensureAuthority();
        if (
          manager.getRoom(roomId) !== captured.room ||
          manager.getParticipant(roomId, command.userId) !== captured.admitted
        )
          return false;
        return manager.leaveRoomIfSocketOwned(
          roomId,
          command.userId,
          captured.participant.socketId,
          captured.participant.voiceEnforcementSessionGeneration
        );
      },
    };
    const delayedSocket = captured.socket;
    const delayedIO: ForceDisconnectIO = {
      sockets: {
        sockets: new Map(
          delayedSocket
            ? [
                [
                  captured.participant.socketId,
                  {
                    emit: (event, ...args) =>
                      active.fanout.push(() => {
                        if (this.socketCurrent(command, captured))
                          delayedSocket.emit(event, ...args);
                      }),
                    disconnect: (close) =>
                      active.fanout.push(() => {
                        if (this.socketCurrent(command, captured)) delayedSocket.disconnect(close);
                      }),
                  },
                ],
              ]
            : []
        ),
      },
    };
    await handleForceDisconnect(
      adapter,
      delayedIO,
      roomId,
      command.userId,
      this.options.emitSecurityEvent,
      { reason: 'access_revoked' }
    );
  }
}
