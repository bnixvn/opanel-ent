import React, { useState } from 'react';
import { api } from './api.js';
import { Message, useMessage } from './components.jsx';
import * as passkeys from './passkey.js';

export default function Login({ brand, onSignedIn }) {
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [code, setCode] = useState('');
  const [needsCode, setNeedsCode] = useState(false);
  const [busy, setBusy] = useState(false);
  const msg = useMessage();

  // Sends the password, and whatever second step the server asked for.
  async function attempt(extra) {
    const body = { username, password };
    if (needsCode && code) body.code = code;
    return api.post('/auth/login', { ...body, ...extra });
  }

  async function submit(e) {
    e.preventDefault();
    setBusy(true);
    msg.clear();
    try {
      const res = await attempt({});
      onSignedIn(res.user || res);
    } catch (err) {
      await handleSecondStep(err);
    } finally {
      setBusy(false);
    }
  }

  // The server replies with a distinct code rather than a message the form
  // has to read, so what is asked for next is the server's decision.
  async function handleSecondStep(err) {
    if (err.code === 'passkey_required') {
      if (!passkeys.supported()) {
        msg.fail(new Error(
          'This account uses a passkey, which this browser cannot offer here. '
          + 'Open the panel over HTTPS with a certificate the browser trusts.',
        ));
        return;
      }
      msg.warn('Confirm with your passkey.');
      try {
        const assertion = await passkeys.assertFrom(err.details);
        const res = await attempt({
          passkey_challenge_id: err.details.challenge_id,
          passkey_credential: assertion,
        });
        onSignedIn(res.user || res);
        return;
      } catch (inner) {
        // Cancelling the browser's prompt is not a failure to shout about.
        if (inner && inner.name === 'NotAllowedError') {
          msg.warn(err.details && err.details.totp_available
            ? 'Passkey cancelled. Enter your two-factor code instead.'
            : 'Passkey cancelled.');
          if (err.details && err.details.totp_available) setNeedsCode(true);
          return;
        }
        msg.fail(inner);
        return;
      }
    }
    if (err.code === 'totp_required' || err.code === 'two_factor_required') {
      setNeedsCode(true);
      msg.warn('Enter the code from your authenticator app.');
      return;
    }
    msg.fail(err);
  }

  return (
    <div className="login">
      <form className="card" onSubmit={submit}>
        <h1 className="brand">
          {brand.logo ? <img src={brand.logo} alt={brand.name} /> : brand.name}
          <small>hosting control panel</small>
        </h1>

        <Message value={msg.message} onClear={msg.clear} />

        <p>
          <label htmlFor="u">Username</label>
          <input
            id="u"
            autoComplete="username"
            required
            value={username}
            onChange={(e) => setUsername(e.target.value)}
          />
        </p>
        <p>
          <label htmlFor="p">Password</label>
          <input
            id="p"
            type="password"
            autoComplete="current-password"
            required
            value={password}
            onChange={(e) => setPassword(e.target.value)}
          />
        </p>
        {needsCode && (
          <p>
            <label htmlFor="c">Two-factor code</label>
            <input
              id="c"
              inputMode="numeric"
              autoComplete="one-time-code"
              value={code}
              onChange={(e) => setCode(e.target.value)}
            />
          </p>
        )}
        <p style={{ marginBottom: 0 }}>
          <button type="submit" className="primary" style={{ width: '100%' }} disabled={busy}>
            {busy ? 'Signing in…' : 'Sign in'}
          </button>
        </p>
      </form>
    </div>
  );
}
