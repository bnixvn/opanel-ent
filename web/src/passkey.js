import { api } from './api.js';

// WebAuthn speaks ArrayBuffers and the API speaks JSON, so every ceremony is
// a pair of conversions. These four helpers are the whole of it.

function fromBase64Url(s) {
  const pad = s.replace(/-/g, '+').replace(/_/g, '/');
  const raw = atob(pad + '==='.slice((pad.length + 3) % 4));
  const out = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i += 1) out[i] = raw.charCodeAt(i);
  return out.buffer;
}

function toBase64Url(buf) {
  const bytes = new Uint8Array(buf);
  let s = '';
  for (let i = 0; i < bytes.length; i += 1) s += String.fromCharCode(bytes[i]);
  return btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

// supported reports whether this browser can do any of it. Chiefly false in
// an insecure context, which is exactly the case the panel warns about.
export function supported() {
  return typeof window !== 'undefined'
    && !!window.PublicKeyCredential
    && !!(navigator.credentials && navigator.credentials.create);
}

// register creates a passkey and hands it to the panel.
export async function register(label) {
  const start = await api.post('/auth/passkeys/register/start', {});
  const opts = start.options.publicKey;

  const created = await navigator.credentials.create({
    publicKey: {
      ...opts,
      challenge: fromBase64Url(opts.challenge),
      user: { ...opts.user, id: fromBase64Url(opts.user.id) },
      excludeCredentials: (opts.excludeCredentials || []).map((c) => ({
        ...c, id: fromBase64Url(c.id),
      })),
    },
  });
  if (!created) throw new Error('No passkey was created.');

  return api.post('/auth/passkeys/register/finish', {
    challenge_id: start.challenge_id,
    label,
    credential: {
      id: created.id,
      rawId: toBase64Url(created.rawId),
      type: created.type,
      clientExtensionResults: created.getClientExtensionResults(),
      response: {
        clientDataJSON: toBase64Url(created.response.clientDataJSON),
        attestationObject: toBase64Url(created.response.attestationObject),
        transports: created.response.getTransports
          ? created.response.getTransports() : [],
      },
    },
  });
}

// signIn asks the browser for whichever passkey matches this site.
//
// No username is sent: the account comes back with the assertion. Asking for
// one first would tell anybody who asked which usernames exist.
export async function signIn() {
  const start = await api.post('/auth/passkey/login/start', {});
  const opts = start.options.publicKey;

  const assertion = await navigator.credentials.get({
    publicKey: {
      ...opts,
      challenge: fromBase64Url(opts.challenge),
      allowCredentials: (opts.allowCredentials || []).map((c) => ({
        ...c, id: fromBase64Url(c.id),
      })),
    },
  });
  if (!assertion) throw new Error('No passkey was offered.');

  return api.post('/auth/passkey/login/finish', {
    challenge_id: start.challenge_id,
    credential: {
      id: assertion.id,
      rawId: toBase64Url(assertion.rawId),
      type: assertion.type,
      clientExtensionResults: assertion.getClientExtensionResults(),
      response: {
        clientDataJSON: toBase64Url(assertion.response.clientDataJSON),
        authenticatorData: toBase64Url(assertion.response.authenticatorData),
        signature: toBase64Url(assertion.response.signature),
        userHandle: assertion.response.userHandle
          ? toBase64Url(assertion.response.userHandle) : '',
      },
    },
  });
}
