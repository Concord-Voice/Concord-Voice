// @vitest-environment node
import { createHash } from 'node:crypto';
import { spawn } from 'node:child_process';
import { createServer as createHttpServer, type Server as HttpServer } from 'node:http';
import { connect as connectNet, type Socket } from 'node:net';
import type { Duplex } from 'node:stream';
import { once } from 'node:events';
import { createRequire } from 'node:module';
import { readFileSync } from 'node:fs';
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { createServer as createHttpsServer, type Server as HttpsServer } from 'node:https';
import { describe, expect, it } from 'vitest';

const fixtureBytes = Buffer.from('electron artifact fixture bytes\n');
const fixtureDigest = createHash('sha256').update(fixtureBytes).digest('hex');
const fakeProxyCredentials = {
  username: 'fixture-user@example.invalid',
  password: ['fake-fixture', ':with/slash'].join(''),
};
const desktopRequire = createRequire(path.resolve(__dirname, '../../../package.json'));
const globalAgentPackage = desktopRequire.resolve('global-agent/package.json', {
  paths: [desktopRequire.resolve('@electron/get/package.json')],
});
const globalAgentRequire = createRequire(globalAgentPackage);
const roarrPackage = globalAgentRequire.resolve('roarr/package.json');
const roarrRequire = createRequire(roarrPackage);

function authenticatedProxy(port: number): { url: string; authorization: string } {
  const proxyUrl = new URL(`http://127.0.0.1:${port}`);
  proxyUrl.username = fakeProxyCredentials.username;
  proxyUrl.password = fakeProxyCredentials.password;
  return {
    url: proxyUrl.toString(),
    authorization: `Basic ${Buffer.from(
      `${fakeProxyCredentials.username}:${fakeProxyCredentials.password}`
    ).toString('base64')}`,
  };
}

const downloadChild = String.raw`
const { downloadArtifact, initializeProxy } = require(process.env.ELECTRON_GET_ENTRY);
initializeProxy();
downloadArtifact({
  isGeneric: true,
  artifactName: 'fixture.bin',
  version: '1.0.0',
  checksums: { 'fixture.bin': process.env.FIXTURE_SHA256 },
  mirrorOptions: { mirror: process.env.FIXTURE_MIRROR },
  cacheRoot: process.env.FIXTURE_CACHE,
  force: true,
  downloadOptions: { timeout: { request: 2500 } },
}).then(async (filePath) => {
  const bytes = require('node:fs').readFileSync(filePath);
  console.error('RESULT:' + JSON.stringify({ ok: true, bytes: bytes.toString('base64') }));
}).catch((error) => {
  console.error('RESULT:' + JSON.stringify({ ok: false, error: error.message }));
  process.exitCode = 1;
});
`;

function listen(server: HttpServer | HttpsServer): Promise<number> {
  server.listen(0, '127.0.0.1');
  return once(server, 'listening').then(() => (server.address() as { port: number }).port);
}

async function close(server: HttpServer | HttpsServer): Promise<void> {
  if (!server.listening) return;
  server.closeAllConnections();
  await new Promise<void>((resolve, reject) =>
    server.close((error) => (error ? reject(error) : resolve()))
  );
}

function runDownload(input: {
  cache: string;
  mirror: string;
  httpProxy?: string;
  httpsProxy?: string;
  noProxy?: string;
  extraCaCert?: string;
}): Promise<{ status: number | null; stdout: string; stderr: string }> {
  const electronGetEntry = desktopRequire.resolve('@electron/get');
  return new Promise((resolve, reject) => {
    const child = spawn(process.execPath, ['-e', downloadChild], {
      env: {
        ...process.env,
        ELECTRON_GET_ENTRY: electronGetEntry,
        FIXTURE_SHA256: fixtureDigest,
        FIXTURE_MIRROR: input.mirror,
        FIXTURE_CACHE: input.cache,
        HTTP_PROXY: input.httpProxy ?? '',
        HTTPS_PROXY: input.httpsProxy ?? '',
        NO_PROXY: input.noProxy ?? '',
        http_proxy: input.httpProxy ?? '',
        https_proxy: input.httpsProxy ?? '',
        no_proxy: input.noProxy ?? '',
        GLOBAL_AGENT_HTTP_PROXY: '',
        GLOBAL_AGENT_HTTPS_PROXY: '',
        GLOBAL_AGENT_NO_PROXY: '',
        global_agent_http_proxy: '',
        global_agent_https_proxy: '',
        global_agent_no_proxy: '',
        ROARR_LOG: 'true',
        ELECTRON_GET_NO_PROGRESS: '1',
        NODE_EXTRA_CA_CERTS: input.extraCaCert ?? '',
        NODE_TLS_REJECT_UNAUTHORIZED: '1',
      },
    });
    let stdout = '';
    let stderr = '';
    const timeout = setTimeout(() => child.kill('SIGKILL'), 5000);
    child.stdout.setEncoding('utf8').on('data', (chunk: string) => (stdout += chunk));
    child.stderr.setEncoding('utf8').on('data', (chunk: string) => (stderr += chunk));
    child.on('error', (error) => {
      clearTimeout(timeout);
      reject(error);
    });
    child.on('close', (status) => {
      clearTimeout(timeout);
      resolve({ status, stdout, stderr });
    });
  });
}

function resultFrom(child: Awaited<ReturnType<typeof runDownload>>): {
  ok: boolean;
  bytes?: string;
  error?: string;
} {
  const line = child.stderr.split('\n').find((entry) => entry.startsWith('RESULT:'));
  expect(
    line,
    `child result missing; stderr=${child.stderr}; stdout=${child.stdout}`
  ).toBeDefined();
  return JSON.parse(line!.slice('RESULT:'.length)) as {
    ok: boolean;
    bytes?: string;
    error?: string;
  };
}

function hasRoarrProxyLog(stdout: string): boolean {
  return stdout.split('\n').some((line) => {
    try {
      const record = JSON.parse(line) as { context?: { namespace?: string }; message?: string };
      return record.context?.namespace === 'Agent' && record.message === 'proxying request';
    } catch {
      return false;
    }
  });
}

describe('@electron/get proxy behavior after removing sprintf-js', () => {
  let cache: string;
  const servers: Array<HttpServer | HttpsServer> = [];
  const proxySockets: Array<Socket | Duplex> = [];

  beforeEach(async () => {
    cache = await mkdtemp(path.join(tmpdir(), 'sprintf-js-proxy-'));
  });

  afterEach(async () => {
    for (const socket of proxySockets.splice(0)) socket.destroy();
    await Promise.all(servers.splice(0).map(close));
    await rm(cache, { recursive: true, force: true });
  });

  it('keeps authenticated HTTP proxy downloads and the original logger working', async () => {
    const requests: string[] = [];
    let expectedAuthorization = '';
    const proxy = createHttpServer((request, response) => {
      requests.push(`${request.url}|${request.headers['proxy-authorization'] ?? ''}`);
      if (request.headers['proxy-authorization'] !== expectedAuthorization) {
        response.writeHead(407).end('proxy authentication required');
        return;
      }
      response.writeHead(200, { 'content-length': fixtureBytes.length }).end(fixtureBytes);
    });
    servers.push(proxy);
    const proxyPort = await listen(proxy);
    const proxyCredentials = authenticatedProxy(proxyPort);
    expectedAuthorization = proxyCredentials.authorization;
    const child = await runDownload({
      cache,
      mirror: 'http://artifact.invalid/',
      httpProxy: proxyCredentials.url,
    });

    expect(child.status, `stderr=${child.stderr}\nstdout=${child.stdout}`).toBe(0);
    expect(resultFrom(child)).toEqual({ ok: true, bytes: fixtureBytes.toString('base64') });
    expect(requests).toHaveLength(1);
    expect(requests[0]).toContain('http://artifact.invalid/v1.0.0/fixture.bin|');
    expect(hasRoarrProxyLog(child.stdout)).toBe(true);
  });

  it('does not resolve sprintf-js beneath global-agent’s roarr dependency', () => {
    const lock = JSON.parse(
      readFileSync(path.resolve(__dirname, '../../../package-lock.json'), 'utf8')
    ) as { packages: Record<string, unknown> };
    const lockedCopies = Object.keys(lock.packages).filter(
      (entry) => entry === 'node_modules/sprintf-js' || entry.endsWith('/node_modules/sprintf-js')
    );
    let installedCopy: string | null = null;
    try {
      installedCopy = roarrRequire.resolve('sprintf-js');
    } catch {
      // The expected fixed tree has no formatter available to this consumer.
    }
    expect({ lockedCopies, installedCopy }).toEqual({ lockedCopies: [], installedCopy: null });
  });

  it('preserves authenticated HTTPS CONNECT downloads with encoded fake credentials', async () => {
    const certModule = desktopRequire('selfsigned') as {
      generate: (
        attributes: object[],
        options: object
      ) => Promise<{ cert: string; private: string }>;
    };
    const pair = await certModule.generate([{ name: 'commonName', value: 'localhost' }], {
      days: 1,
      keyType: 'ec',
      curve: 'P-256',
      extensions: [{ name: 'subjectAltName', altNames: [{ type: 2, value: 'localhost' }] }],
    });
    const caPath = path.join(cache, 'fixture-ca.pem');
    await writeFile(caPath, pair.cert);
    const origin = createHttpsServer(
      { key: pair.private, cert: pair.cert },
      (_request, response) => {
        response.writeHead(200, { 'content-length': fixtureBytes.length }).end(fixtureBytes);
      }
    );
    servers.push(origin);
    const originPort = await listen(origin);
    const connects: string[] = [];
    let expectedAuthorization = '';
    const proxy = createHttpServer();
    proxy.on('connect', (request, client, head) => {
      connects.push(`${request.url}|${request.headers['proxy-authorization'] ?? ''}`);
      if (request.headers['proxy-authorization'] !== expectedAuthorization) {
        client.end('HTTP/1.1 407 Proxy Authentication Required\r\n\r\n');
        return;
      }
      const upstream = connectNet(originPort, '127.0.0.1');
      proxySockets.push(upstream, client);
      upstream.once('connect', () => {
        client.write('HTTP/1.1 200 Connection Established\r\n\r\n');
        if (head.length) upstream.write(head);
        client.pipe(upstream).pipe(client);
      });
    });
    servers.push(proxy);
    const proxyPort = await listen(proxy);
    const proxyCredentials = authenticatedProxy(proxyPort);
    expectedAuthorization = proxyCredentials.authorization;
    const child = await runDownload({
      cache,
      mirror: `https://localhost:${originPort}/`,
      httpsProxy: proxyCredentials.url,
      extraCaCert: caPath,
    });

    expect(child.status, `stderr=${child.stderr}\nstdout=${child.stdout}`).toBe(0);
    expect(resultFrom(child)).toEqual({ ok: true, bytes: fixtureBytes.toString('base64') });
    expect(connects).toEqual([`localhost:${originPort}|${proxyCredentials.authorization}`]);
    expect(hasRoarrProxyLog(child.stdout)).toBe(true);
  });

  it('honors NO_PROXY by reaching the local origin directly', async () => {
    let originHits = 0;
    const origin = createHttpServer((_request, response) => {
      originHits += 1;
      response.writeHead(200, { 'content-length': fixtureBytes.length }).end(fixtureBytes);
    });
    servers.push(origin);
    const originPort = await listen(origin);
    let proxyHits = 0;
    const proxy = createHttpServer((_request, response) => {
      proxyHits += 1;
      response.writeHead(502).end('NO_PROXY was ignored');
    });
    servers.push(proxy);
    const proxyPort = await listen(proxy);
    const child = await runDownload({
      cache,
      mirror: `http://127.0.0.1:${originPort}/`,
      httpProxy: `http://127.0.0.1:${proxyPort}`,
      noProxy: '127.0.0.1',
    });

    expect(child.status).toBe(0);
    expect(resultFrom(child)).toEqual({ ok: true, bytes: fixtureBytes.toString('base64') });
    expect(originHits).toBe(1);
    expect(proxyHits).toBe(0);
  });

  it('surfaces proxy rejection instead of silently downloading directly', async () => {
    const requests: string[] = [];
    const proxy = createHttpServer((request, response) => {
      requests.push(request.url ?? '');
      response.writeHead(407).end('proxy authentication required');
    });
    servers.push(proxy);
    const proxyPort = await listen(proxy);
    const child = await runDownload({
      cache,
      mirror: 'http://artifact.invalid/',
      httpProxy: `http://127.0.0.1:${proxyPort}`,
    });

    expect(child.status).not.toBe(0);
    expect(resultFrom(child)).toMatchObject({ ok: false });
    expect(requests).toEqual(['http://artifact.invalid/v1.0.0/fixture.bin']);
    expect(hasRoarrProxyLog(child.stdout)).toBe(true);
  });

  it('surfaces an unreachable proxy instead of falling back to the reachable origin', async () => {
    let originHits = 0;
    const origin = createHttpServer((_request, response) => {
      originHits += 1;
      response.writeHead(200, { 'content-length': fixtureBytes.length }).end(fixtureBytes);
    });
    servers.push(origin);
    const originPort = await listen(origin);

    const proxy = createHttpServer();
    servers.push(proxy);
    const proxyPort = await listen(proxy);
    await close(proxy);

    const child = await runDownload({
      cache,
      mirror: `http://127.0.0.1:${originPort}/`,
      httpProxy: `http://127.0.0.1:${proxyPort}`,
    });

    expect(child.status).not.toBe(0);
    expect(resultFrom(child).error).toContain('ECONNREFUSED');
    expect(originHits).toBe(0);
  });

  it('rejects an untrusted TLS certificate through the configured proxy', async () => {
    const certModule = desktopRequire('selfsigned') as {
      generate: (
        attributes: object[],
        options: object
      ) => Promise<{ cert: string; private: string }>;
    };
    const pair = await certModule.generate([{ name: 'commonName', value: 'localhost' }], {
      days: 1,
      keyType: 'ec',
      curve: 'P-256',
    });
    const origin = createHttpsServer(
      { key: pair.private, cert: pair.cert },
      (_request, response) => {
        response.writeHead(200, { 'content-length': fixtureBytes.length }).end(fixtureBytes);
      }
    );
    servers.push(origin);
    const originPort = await listen(origin);
    const connects: string[] = [];
    const proxy = createHttpServer();
    proxy.on('connect', (request, client, head) => {
      connects.push(request.url ?? '');
      const upstream = connectNet(originPort, '127.0.0.1');
      proxySockets.push(upstream, client);
      upstream.once('connect', () => {
        client.write('HTTP/1.1 200 Connection Established\r\n\r\n');
        if (head.length) upstream.write(head);
        client.pipe(upstream).pipe(client);
      });
    });
    servers.push(proxy);
    const proxyPort = await listen(proxy);
    const child = await runDownload({
      cache,
      mirror: `https://localhost:${originPort}/`,
      httpsProxy: `http://127.0.0.1:${proxyPort}`,
    });

    expect(child.status).not.toBe(0);
    expect(resultFrom(child)).toMatchObject({ ok: false });
    expect(connects).toEqual([`localhost:${originPort}`]);
  });
});
