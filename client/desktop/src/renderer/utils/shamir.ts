/**
 * Shamir's Secret Sharing over GF(256)
 *
 * Splits a secret byte array into N shares with a K-of-N threshold.
 * Any K shares can reconstruct the secret; fewer reveals nothing.
 *
 * Each byte of the secret is split independently using random polynomials
 * of degree K-1 over the Galois Field GF(2^8) with the irreducible
 * polynomial x^8 + x^4 + x^3 + x + 1 (0x11B, same as AES).
 */

// GF(256) log and exp tables for fast multiplication
const EXP_TABLE = new Uint8Array(512);
const LOG_TABLE = new Uint8Array(256);

// Initialize tables using generator 3
(function initTables() {
  let x = 1;
  for (let i = 0; i < 255; i++) {
    EXP_TABLE[i] = x;
    LOG_TABLE[x] = i;
    x = x ^ (x << 1) ^ (x & 0x80 ? 0x11b : 0);
    x &= 0xff;
  }
  // Extend exp table for easy wraparound
  for (let i = 255; i < 512; i++) {
    EXP_TABLE[i] = EXP_TABLE[i - 255];
  }
})();

function gfMul(a: number, b: number): number {
  if (a === 0 || b === 0) return 0;
  return EXP_TABLE[LOG_TABLE[a] + LOG_TABLE[b]];
}

function gfDiv(a: number, b: number): number {
  if (b === 0) throw new Error('Division by zero in GF(256)');
  if (a === 0) return 0;
  return EXP_TABLE[(LOG_TABLE[a] - LOG_TABLE[b] + 255) % 255];
}

/**
 * Evaluate a polynomial at point x in GF(256).
 * coeffs[0] is the constant term (the secret byte).
 */
function evaluatePolynomial(coeffs: Uint8Array, x: number): number {
  let result = 0;
  for (let i = coeffs.length - 1; i >= 0; i--) {
    result = gfMul(result, x) ^ coeffs[i];
  }
  return result;
}

/**
 * Split a secret into n shares with threshold k.
 * Returns n shares, each containing { index (1..n), data (same length as secret) }.
 */
export function split(
  secret: Uint8Array,
  n: number,
  k: number
): Array<{ index: number; data: Uint8Array }> {
  if (k < 2) throw new Error('Threshold must be at least 2');
  if (n < k) throw new Error('Number of shares must be >= threshold');
  if (n > 255) throw new Error('Maximum 255 shares');

  const shares: Array<{ index: number; data: Uint8Array }> = [];
  for (let i = 0; i < n; i++) {
    shares.push({ index: i + 1, data: new Uint8Array(secret.length) });
  }

  // For each byte of the secret, create a random polynomial and evaluate
  const coeffs = new Uint8Array(k);
  for (let byteIdx = 0; byteIdx < secret.length; byteIdx++) {
    // coeffs[0] = secret byte, coeffs[1..k-1] = random
    coeffs[0] = secret[byteIdx];
    crypto.getRandomValues(coeffs.subarray(1));

    // Evaluate at points 1..n
    for (let i = 0; i < n; i++) {
      shares[i].data[byteIdx] = evaluatePolynomial(coeffs, i + 1);
    }
  }

  return shares;
}

/**
 * Reconstruct the secret from k or more shares using Lagrange interpolation.
 */
export function combine(shares: Array<{ index: number; data: Uint8Array }>): Uint8Array {
  if (shares.length < 2) throw new Error('Need at least 2 shares');

  const secretLength = shares[0].data.length;
  const result = new Uint8Array(secretLength);

  // Lagrange interpolation at x=0 for each byte position
  for (let byteIdx = 0; byteIdx < secretLength; byteIdx++) {
    let value = 0;
    for (let i = 0; i < shares.length; i++) {
      const xi = shares[i].index;
      let lagrange = 1;

      for (let j = 0; j < shares.length; j++) {
        if (i === j) continue;
        const xj = shares[j].index;
        // lagrange *= xj / (xj ^ xi)  in GF(256)
        // Note: subtraction in GF(256) is XOR
        lagrange = gfMul(lagrange, gfDiv(xj, xi ^ xj));
      }

      value ^= gfMul(shares[i].data[byteIdx], lagrange);
    }
    result[byteIdx] = value;
  }

  return result;
}
