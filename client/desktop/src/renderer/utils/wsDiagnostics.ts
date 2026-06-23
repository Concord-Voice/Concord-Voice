import { errorMessage } from './redactError';

type ServerErrorPayload = {
  code?: unknown;
  channel_id?: unknown;
  current_epoch?: unknown;
};

export type WsDiagnosticSummary = {
  type: string;
  event?: string;
  code?: string | number;
  reason?: string;
  message?: string;
  channelId?: string;
  currentEpoch?: number;
};

export function summarizeWsDiagnostic(value: unknown): WsDiagnosticSummary {
  if (value instanceof CloseEvent) {
    return {
      type: 'CloseEvent',
      event: value.type,
      code: value.code,
      ...(value.reason ? { reason: value.reason } : {}),
    };
  }

  if (value instanceof Error) {
    return {
      type: value.name || 'Error',
      message: errorMessage(value),
    };
  }

  if (value instanceof Event) {
    return {
      type: value.constructor.name || 'Event',
      event: value.type,
    };
  }

  return {
    type: typeof value,
  };
}

export function summarizeWsServerError(data: ServerErrorPayload): WsDiagnosticSummary {
  const code = typeof data.code === 'string' && data.code ? data.code : 'unknown';
  return {
    type: 'server_error',
    code,
    ...(typeof data.channel_id === 'string' ? { channelId: data.channel_id } : {}),
    ...(typeof data.current_epoch === 'number' ? { currentEpoch: data.current_epoch } : {}),
  };
}
