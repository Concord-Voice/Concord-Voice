import { spawn } from 'node:child_process';
import path from 'node:path';

const ALLOWED_INTERMEDIATE_CN_PREFIX = 'Microsoft ID Verified CS ';

export interface CertInfo {
  subjectCN: string;
  issuerCN: string;
}

/**
 * Extract the full Authenticode cert chain from a signed PE file using
 * PowerShell's Get-AuthenticodeSignature. Returns the leaf cert plus each
 * intermediate. On non-Windows returns an empty array.
 *
 * Leaf-thumbprint pinning is explicitly rejected here: Microsoft Trusted
 * Signing leaves are 72-hour short-lived and auto-renewed daily. Any
 * future attempt to "tighten" this must also automate rotation.
 */
// Resolve via %SystemRoot% (with C:\Windows fallback) instead of hardcoding
// the drive — ARM64 Windows and enterprise imaging can relocate the system
// root. Absolute-path resolution still satisfies SonarQube S4036.
const SYSTEM_ROOT = process.env.SystemRoot ?? String.raw`C:\Windows`;
const WINDOWS_POWERSHELL_PATH = path.join(
  SYSTEM_ROOT,
  'System32',
  'WindowsPowerShell',
  'v1.0',
  'powershell.exe'
);

export async function extractChain(filePath: string): Promise<CertInfo[]> {
  if (process.platform !== 'win32') return [];
  const escapedPath = filePath.replaceAll("'", "''");
  const script = `
    $ErrorActionPreference = 'Stop'
    $sig = Get-AuthenticodeSignature -FilePath '${escapedPath}'
    if ($sig.Status -ne 'Valid') { throw "Authenticode status: $($sig.Status)" }
    $chain = New-Object System.Security.Cryptography.X509Certificates.X509Chain
    [void]$chain.Build($sig.SignerCertificate)
    $nameType = [System.Security.Cryptography.X509Certificates.X509NameType]::SimpleName
    $chain.ChainElements | ForEach-Object {
      [PSCustomObject]@{
        subjectCN = $_.Certificate.GetNameInfo($nameType, $false)
        issuerCN  = $_.Certificate.GetNameInfo($nameType, $true)
      }
    } | ConvertTo-Json -Depth 2
  `;

  return new Promise((resolve, reject) => {
    const proc = spawn(WINDOWS_POWERSHELL_PATH, ['-NoProfile', '-Command', script], {
      timeout: 30_000,
    });
    let stdout = '';
    let stderr = '';
    proc.stdout.on('data', (d) => {
      stdout += d.toString('utf-8');
    });
    proc.stderr.on('data', (d) => {
      stderr += d.toString('utf-8');
    });
    proc.on('error', (err) => {
      reject(new Error(`spawn powershell failed: ${err.message}`));
    });
    proc.on('close', (code) => {
      if (code !== 0) {
        reject(new Error(`extractChain failed (exit ${code}): ${stderr || stdout}`));
        return;
      }
      if (!stdout.trim()) {
        reject(new Error('extractChain: empty PowerShell output'));
        return;
      }
      try {
        const parsed = JSON.parse(stdout) as CertInfo | CertInfo[] | null;
        if (parsed == null) {
          reject(new Error('extractChain: null chain from PowerShell'));
          return;
        }
        resolve(Array.isArray(parsed) ? parsed : [parsed]);
      } catch (e) {
        reject(new Error(`extractChain: invalid JSON — ${(e as Error).message}`));
      }
    });
  });
}

/**
 * Verify the signature chain against allowed publishers and the pinned
 * Microsoft Trusted Signing intermediate issuer. Pure function over
 * a chain — extraction is a separate concern so this can be unit tested.
 */
export function verifyChain(
  chain: CertInfo[],
  allowedPublishers: readonly string[]
): string | null {
  if (chain.length === 0) return 'empty cert chain';
  if (chain.length < 2) {
    return 'chain too short: leaf-only chain cannot be validated against pinned intermediate';
  }
  const leaf = chain[0];
  if (!allowedPublishers.includes(leaf.subjectCN)) {
    return `unauthorized publisher: ${leaf.subjectCN}`;
  }
  if (!leaf.issuerCN.startsWith(ALLOWED_INTERMEDIATE_CN_PREFIX)) {
    return `leaf not issued by pinned Microsoft Trusted Signing intermediate (issuer: ${leaf.issuerCN})`;
  }
  return null;
}
