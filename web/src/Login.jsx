import React, { useState } from 'react';
import { api } from './api.js';
import { Message, useMessage } from './components.jsx';

export default function Login({ brand, onSignedIn }) {
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [code, setCode] = useState('');
  const [needsCode, setNeedsCode] = useState(false);
  const [busy, setBusy] = useState(false);
  const msg = useMessage();

  async function submit(e) {
    e.preventDefault();
    setBusy(true);
    msg.clear();
    try {
      const body = { username, password };
      if (needsCode && code) body.code = code;
      const res = await api.post('/auth/login', body);
      onSignedIn(res.user || res);
    } catch (err) {
      // The server asks for a second factor with its own code, so the form
      // knows to show the field without guessing from the message text.
      if (err.code === 'totp_required' || err.code === 'two_factor_required') {
        setNeedsCode(true);
        msg.warn('Enter the code from your authenticator app.');
      } else {
        msg.fail(err);
      }
    } finally {
      setBusy(false);
    }
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
        <p>
          <button type="submit" className="primary" style={{ width: '100%' }} disabled={busy}>
            {busy ? 'Signing in…' : 'Sign in'}
          </button>
        </p>
      </form>
    </div>
  );
}
