import { execFile } from 'node:child_process';
import { isAbsolute } from 'node:path';
import { fileURLToPath } from 'node:url';

export const natsServerAcquisitionTimeoutMs = 120_000;
const resolver = fileURLToPath(
  new URL('../../../../scripts/test-tools/nats-server.sh', import.meta.url)
);

export function resolveNatsServerBinary(): Promise<string> {
  return new Promise((resolve, reject) => {
    const child = execFile(
      'bash',
      [resolver],
      {
        timeout: natsServerAcquisitionTimeoutMs - 5000,
        killSignal: 'SIGTERM',
        maxBuffer: 64 * 1024,
      },
      (error, stdout, stderr) => {
        clearTimeout(graceDeadline);
        if (error) {
          reject(
            new Error(
              `Mandatory NATS test broker acquisition failed: ${stderr.trim() || error.message}`,
              {
                cause: error,
              }
            )
          );
          return;
        }
        const binary = stdout.trim();
        if (!isAbsolute(binary) || binary.includes('\n') || binary.includes('\r')) {
          reject(new Error('NATS test broker resolver must return exactly one executable path'));
          return;
        }
        resolve(binary);
      }
    );
    const graceDeadline = setTimeout(() => {
      child.kill('SIGKILL');
      reject(
        new Error('Mandatory NATS test broker acquisition exceeded its 120-second cleanup deadline')
      );
    }, natsServerAcquisitionTimeoutMs);
  });
}
