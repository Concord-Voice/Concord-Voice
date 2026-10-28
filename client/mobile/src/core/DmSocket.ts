/**
 * One-conversation DM socket, carried over from the #663 probe.
 * Frames are validated here, at the dispatch boundary, before anything uses them.
 */

export type DmMessage = {
  id: string;
  conversationId: string;
  userId: string;
  content: string;
  keyVersion: number;
};

type Waiter<T> = {
  accept: (frame: Frame) => T | undefined;
  resolve: (value: T) => void;
  reject: (error: Error) => void;
};
type Frame = { type: string; data: Record<string, unknown> };

export type SocketLike = {
  send(data: string): void;
  close(): void;
  onopen: (() => void) | null;
  onmessage: ((event: { data: unknown }) => void) | null;
  onerror: (() => void) | null;
  onclose: (() => void) | null;
};

function parseFrame(raw: unknown): Frame | null {
  if (typeof raw !== 'string') return null;
  try {
    const value: unknown = JSON.parse(raw);
    if (typeof value !== 'object' || value === null) return null;
    const { type, data } = value as Record<string, unknown>;
    if (typeof type !== 'string' || typeof data !== 'object' || data === null)
      return null;
    return { type, data: data as Record<string, unknown> };
  } catch {
    return null;
  }
}

function dmMessage(frame: Frame): DmMessage | undefined {
  if (frame.type !== 'dm_message') return undefined;
  const d = frame.data;
  if (
    typeof d.id !== 'string' ||
    typeof d.conversation_id !== 'string' ||
    typeof d.user_id !== 'string' ||
    typeof d.content !== 'string' ||
    !Number.isSafeInteger(d.key_version) ||
    (d.key_version as number) < 1
  ) {
    return undefined;
  }
  return {
    id: d.id,
    conversationId: d.conversation_id,
    userId: d.user_id,
    content: d.content,
    keyVersion: d.key_version as number,
  };
}

export class DmSocket {
  private waiters: Array<Waiter<unknown>> = [];

  private constructor(
    private readonly socket: SocketLike,
    private readonly timeoutMs: number,
  ) {
    socket.onmessage = event => {
      const frame = parseFrame(event.data);
      if (!frame) return;
      if (frame.type === 'error') {
        this.failAll(new Error('Server error frame'));
        return;
      }
      for (const waiter of [...this.waiters]) {
        const value = waiter.accept(frame);
        if (value !== undefined) {
          this.waiters = this.waiters.filter(w => w !== waiter);
          waiter.resolve(value);
        }
      }
    };
    socket.onclose = () => this.failAll(new Error('Socket closed'));
  }

  static open(
    url: string,
    create: (url: string) => SocketLike,
    timeoutMs = 15000,
  ): Promise<DmSocket> {
    return new Promise((resolve, reject) => {
      const socket = create(url);
      // A socket that never opened is closed and silenced, so no late onopen yields an unowned connection.
      const fail = (message: string) => {
        clearTimeout(timer);
        socket.onopen = null;
        socket.onerror = null;
        socket.close();
        reject(new Error(message));
      };
      const timer = setTimeout(() => fail('Socket open timed out'), timeoutMs);
      socket.onerror = () => fail('Socket failed to open');
      socket.onopen = () => {
        clearTimeout(timer);
        socket.onerror = null;
        resolve(new DmSocket(socket, timeoutMs));
      };
    });
  }

  private failAll(error: Error): void {
    const pending = this.waiters;
    this.waiters = [];
    pending.forEach(w => w.reject(error));
  }

  private wait<T>(
    accept: (frame: Frame) => T | undefined,
    label: string,
  ): Promise<T> {
    return new Promise<T>((resolve, reject) => {
      const timer = setTimeout(() => {
        this.waiters = this.waiters.filter(w => w !== waiter);
        reject(new Error(`Timed out waiting for ${label}`));
      }, this.timeoutMs);
      const waiter: Waiter<T> = {
        accept,
        resolve: v => {
          clearTimeout(timer);
          resolve(v);
        },
        reject: e => {
          clearTimeout(timer);
          reject(e);
        },
      };
      this.waiters.push(waiter as Waiter<unknown>);
    });
  }

  private sendFrame(type: string, data: Record<string, unknown>): void {
    this.socket.send(JSON.stringify({ type, data }));
  }

  async subscribe(conversationId: string): Promise<void> {
    // Send before registering the waiter: a throwing send leaves nothing orphaned,
    // and the reply cannot arrive before this synchronous block finishes.
    this.sendFrame('subscribe_dm', { conversation_id: conversationId });
    await this.wait(
      f =>
        f.type === 'dm_subscribed' && f.data.conversation_id === conversationId
          ? true
          : undefined,
      'dm_subscribed',
    );
  }

  /** Sends ciphertext and resolves with the server-assigned message id. */
  async send(
    conversationId: string,
    ciphertext: string,
    keyVersion: number,
    nonce: string,
  ): Promise<string> {
    this.sendFrame('dm_message', {
      conversation_id: conversationId,
      content: ciphertext,
      key_version: keyVersion,
      nonce,
    });
    return this.wait(
      f =>
        f.type === 'dm_message_ack' &&
        f.data.nonce === nonce &&
        typeof f.data.id === 'string'
          ? f.data.id
          : undefined,
      'dm_message_ack',
    );
  }

  nextMessage(conversationId: string, fromUserId: string): Promise<DmMessage> {
    return this.wait(f => {
      const message = dmMessage(f);
      return message &&
        message.conversationId === conversationId &&
        message.userId === fromUserId
        ? message
        : undefined;
    }, 'dm_message');
  }

  close(): void {
    this.socket.onclose = null;
    this.failAll(new Error('Socket closed'));
    this.socket.close();
  }
}
