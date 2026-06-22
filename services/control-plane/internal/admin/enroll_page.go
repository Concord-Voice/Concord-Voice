package admin

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// EnrollPage serves the minimal, FUNCTIONAL (deliberately unstyled — #1691 brands
// it) admin enrollment page (#1688 §14). It drives the two-step WebAuthn
// registration ceremony entirely client-side:
//
//  1. The operator pastes the password and (pre-filled from the URL) username +
//     token, then clicks "Register key".
//  2. JS POSTs /admin/api/v1/enroll/begin, receives the creation options + an
//     opaque handle, runs navigator.credentials.create(), then POSTs the
//     attestation to /admin/api/v1/enroll/finish.
//
// SECURITY: the served HTML is a server-FIXED constant — there is NO template
// interpolation of any request value, so it cannot be an injection sink. The
// username/token are read by client-side JS from the URL query (they are not
// secrets-at-rest: the token is single-use + 1h TTL and is consumed server-side).
// All API URLs are root-relative ("/admin/api/v1/...") and resolve against the
// admin console origin.
func (h *Handler) EnrollPage(c *gin.Context) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	// Conservative CSP: this page runs only its own inline script and talks only
	// to its own origin. No external resources, no framing.
	c.Header("Content-Security-Policy",
		"default-src 'none'; script-src 'unsafe-inline'; connect-src 'self'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	c.String(http.StatusOK, enrollPageHTML)
}

// enrollPageHTML is the fixed enrollment page. No request value is interpolated.
const enrollPageHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Concord Admin — Enroll Hardware Key</title>
</head>
<body>
<h1>Concord Admin — Enroll Hardware Key</h1>
<p>Register your FIDO2 hardware key to activate your admin account. You must
complete this within the one-hour token window. Register a backup key too.</p>

<form id="enroll-form" autocomplete="off">
  <p>
    <label>Username<br>
      <input id="username" name="username" type="text" required>
    </label>
  </p>
  <p>
    <label>Password<br>
      <input id="password" name="password" type="password" required>
    </label>
  </p>
  <p>
    <label>Enrollment token<br>
      <input id="token" name="token" type="text" required>
    </label>
  </p>
  <p>
    <label>Key name (optional)<br>
      <input id="credential_name" name="credential_name" type="text">
    </label>
  </p>
  <p><button id="submit" type="submit">Register key</button></p>
</form>
<p id="status" role="status" aria-live="polite"></p>

<script>
(function () {
  "use strict";

  // Base64URL <-> ArrayBuffer helpers for the WebAuthn wire encoding.
  function b64urlToBuf(s) {
    s = s.replace(/-/g, "+").replace(/_/g, "/");
    var pad = s.length % 4;
    if (pad) { s += "====".slice(pad); }
    var bin = atob(s);
    var buf = new Uint8Array(bin.length);
    for (var i = 0; i < bin.length; i++) { buf[i] = bin.charCodeAt(i); }
    return buf.buffer;
  }
  function bufToB64url(buf) {
    var bytes = new Uint8Array(buf);
    var bin = "";
    for (var i = 0; i < bytes.length; i++) { bin += String.fromCharCode(bytes[i]); }
    return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  }

  // Pre-fill username + token from the URL query (NOT a secret store; the token
  // is single-use and consumed server-side on the begin call).
  var params = new URLSearchParams(window.location.search);
  if (params.get("username")) { document.getElementById("username").value = params.get("username"); }
  if (params.get("token")) { document.getElementById("token").value = params.get("token"); }

  function setStatus(msg) { document.getElementById("status").textContent = msg; }

  function decodeCreationOptions(publicKey) {
    publicKey.challenge = b64urlToBuf(publicKey.challenge);
    publicKey.user.id = b64urlToBuf(publicKey.user.id);
    if (publicKey.excludeCredentials) {
      publicKey.excludeCredentials = publicKey.excludeCredentials.map(function (c) {
        return { type: c.type, id: b64urlToBuf(c.id), transports: c.transports };
      });
    }
    return publicKey;
  }

  function encodeAttestation(cred) {
    return {
      id: cred.id,
      rawId: bufToB64url(cred.rawId),
      type: cred.type,
      response: {
        attestationObject: bufToB64url(cred.response.attestationObject),
        clientDataJSON: bufToB64url(cred.response.clientDataJSON)
      }
    };
  }

  document.getElementById("enroll-form").addEventListener("submit", function (ev) {
    ev.preventDefault();
    setStatus("Starting registration...");

    var body = {
      username: document.getElementById("username").value,
      password: document.getElementById("password").value,
      token: document.getElementById("token").value
    };

    fetch("/admin/api/v1/enroll/begin", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body)
    }).then(function (res) {
      if (!res.ok) { throw new Error("Begin failed (" + res.status + ")"); }
      return res.json();
    }).then(function (data) {
      var handle = data.handle;
      var publicKey = decodeCreationOptions(data.publicKey.publicKey);
      setStatus("Touch your hardware key...");
      return navigator.credentials.create({ publicKey: publicKey }).then(function (cred) {
        return fetch("/admin/api/v1/enroll/finish", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            handle: handle,
            attestation: encodeAttestation(cred),
            credential_name: document.getElementById("credential_name").value
          })
        });
      });
    }).then(function (res) {
      if (!res.ok) { throw new Error("Finish failed (" + res.status + ")"); }
      setStatus("Success. Your hardware key is registered and your account is active.");
    }).catch(function (err) {
      setStatus("Enrollment failed: " + err.message);
    });
  });
})();
</script>
</body>
</html>`
