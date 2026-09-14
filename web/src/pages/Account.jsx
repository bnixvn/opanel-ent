import React, { useCallback, useEffect, useState } from 'react';
import { api, fmtDate } from '../api.js';
import { Card, Empty, Message, Secret, Tag, useMessage } from '../components.jsx';
import * as passkeys from '../passkey.js';

export default function Account({ me, onChanged }) {
  const [email, setEmail] = useState(me.email || '');
  const [emailPassword, setEmailPassword] = useState('');
  const [current, setCurrent] = useState('');
  const [next, setNext] = useState('');
  const [totp, setTotp] = useState(null);
  const [code, setCode] = useState('');
  const [recovery, setRecovery] = useState([]);
  const [notifications, setNotifications] = useState(null);
  const msg = useMessage();

  useEffect(() => {
    api.get('/notifications').then((r) => setNotifications(r.notifications)).catch(() => {});
  }, []);

  return (
    <>
      <Message value={msg.message} onClear={msg.clear} />

      <Card title="Your account">
        <dl className="kv">
          <dt>Username</dt><dd>{me.username}</dd>
          <dt>Role</dt><dd>{me.role}</dd>
          <dt>Email</dt><dd>{me.email || <span className="muted">not set</span>}</dd>
          <dt>Two-factor</dt>
          <dd>{me.totp_enabled ? <Tag kind="ok">on</Tag> : <Tag>off</Tag>}</dd>
        </dl>
      </Card>

      <Passkeys msg={msg} />

      <Card title="Contact email">
        <p className="muted" style={{ marginTop: 0, fontSize: '.85rem' }}>
          Where the certificate authority sends the warning before one of your
          certificates expires. Changing it needs your password, because
          somebody who could change it from an unlocked browser could take the
          account over quietly.
        </p>
        <form
          className="row"
          onSubmit={async (e) => {
            e.preventDefault();
            msg.clear();
            try {
              const res = await api.post('/auth/email', {
                email: email.trim(), password: emailPassword,
              });
              msg.ok(res.email ? `Email set to ${res.email}` : 'Email cleared');
              setEmailPassword('');
              if (onChanged) onChanged({ ...me, email: res.email });
            } catch (err) {
              msg.fail(err);
            }
          }}
        >
          <div className="field">
            <label htmlFor="acctEmail">Email</label>
            <input
              id="acctEmail"
              type="email"
              autoComplete="email"
              placeholder="you@example.com"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
            />
          </div>
          <div className="field">
            <label htmlFor="acctEmailPw">Your password</label>
            <input
              id="acctEmailPw"
              type="password"
              autoComplete="current-password"
              required
              value={emailPassword}
              onChange={(e) => setEmailPassword(e.target.value)}
            />
          </div>
          <div><button type="submit" className="primary">Save email</button></div>
        </form>
      </Card>

      <Card title="Change your password">
        <form
          className="row"
          onSubmit={async (e) => {
            e.preventDefault();
            msg.clear();
            try {
              await api.post('/auth/password', { current, new: next });
              msg.ok('Password changed');
              setCurrent('');
              setNext('');
            } catch (err) {
              msg.fail(err);
            }
          }}
        >
          <div className="field">
            <label htmlFor="cur">Current password</label>
            <input
              id="cur"
              type="password"
              autoComplete="current-password"
              required
              value={current}
              onChange={(e) => setCurrent(e.target.value)}
            />
          </div>
          <div className="field">
            <label htmlFor="new">New password</label>
            <input
              id="new"
              type="password"
              autoComplete="new-password"
              required
              minLength={10}
              value={next}
              onChange={(e) => setNext(e.target.value)}
            />
          </div>
          <div><button type="submit" className="primary">Change password</button></div>
        </form>
      </Card>

      <Card title="Two-factor authentication">
        {me.totp_enabled ? (
          <>
            <p className="muted" style={{ marginTop: 0 }}>
              Two-factor is on. Turning it off means a stolen password is enough
              to reach this account.
            </p>
            <form
              className="row"
              onSubmit={async (e) => {
                e.preventDefault();
                msg.clear();
                try {
                  await api.post('/auth/2fa/disable', { code });
                  msg.ok('Two-factor turned off. Sign in again to refresh this page.');
                  setCode('');
                } catch (err) {
                  msg.fail(err);
                }
              }}
            >
              <div className="field" style={{ flex: '0 0 10rem' }}>
                <label htmlFor="offCode">Current code</label>
                <input id="offCode" inputMode="numeric" required value={code} onChange={(e) => setCode(e.target.value)} />
              </div>
              <div><button type="submit" className="danger">Turn off</button></div>
            </form>
          </>
        ) : (
          <>
            {!totp ? (
              <button
                type="button"
                className="primary"
                onClick={async () => {
                  msg.clear();
                  try {
                    setTotp(await api.post('/auth/2fa/setup', {}));
                  } catch (err) {
                    msg.fail(err);
                  }
                }}
              >
                Set up two-factor
              </button>
            ) : (
              <>
                <p className="muted" style={{ marginTop: 0 }}>
                  Add this secret to an authenticator app, then enter the code it
                  shows to finish.
                </p>
                <Secret label="Secret" value={totp.secret} />
                {totp.uri && (
                  <p className="muted" style={{ fontSize: '.8rem', wordBreak: 'break-all' }}>
                    {totp.uri}
                  </p>
                )}
                <form
                  className="row"
                  onSubmit={async (e) => {
                    e.preventDefault();
                    msg.clear();
                    try {
                      const res = await api.post('/auth/2fa/enable', { code });
                      setRecovery(res.recovery_codes || []);
                      setTotp(null);
                      setCode('');
                      msg.ok('Two-factor is on. Sign in again to refresh this page.');
                    } catch (err) {
                      msg.fail(err);
                    }
                  }}
                >
                  <div className="field" style={{ flex: '0 0 10rem' }}>
                    <label htmlFor="onCode">Code from the app</label>
                    <input id="onCode" inputMode="numeric" required value={code} onChange={(e) => setCode(e.target.value)} />
                  </div>
                  <div><button type="submit" className="primary">Turn on</button></div>
                </form>
              </>
            )}
            {recovery.length > 0 && (
              <div style={{ marginTop: '1rem' }}>
                <p className="muted" style={{ fontSize: '.85rem' }}>
                  Recovery codes, each usable once. Save them somewhere other than
                  the device with the authenticator on it.
                </p>
                <code style={{ display: 'block', whiteSpace: 'pre-wrap' }}>
                  {recovery.join('\n')}
                </code>
              </div>
            )}
          </>
        )}
      </Card>

      {(me.role === 'admin' || me.role === 'reseller') && (
        <Card title="Alerts">
          <NotificationForm current={notifications} msg={msg} onSaved={setNotifications} />
        </Card>
      )}
    </>
  );
}

function NotificationForm({ current, msg, onSaved }) {
  const [token, setToken] = useState('');
  const [chat, setChat] = useState('');
  const [enabled, setEnabled] = useState(true);

  useEffect(() => {
    if (current) {
      setChat(current.telegram_chat_id || '');
      setEnabled(current.enabled);
    }
  }, [current]);

  return (
    <form
      className="row"
      onSubmit={async (e) => {
        e.preventDefault();
        msg.clear();
        try {
          await api.put('/notifications', {
            telegram_token: token,
            telegram_chat_id: chat,
            events: '',
            enabled,
          });
          setToken('');
          msg.ok('Alert settings saved');
          onSaved((await api.get('/notifications')).notifications);
        } catch (err) {
          msg.fail(err);
        }
      }}
    >
      <div className="field">
        <label htmlFor="tgToken">Telegram bot token</label>
        <input
          id="tgToken"
          type="password"
          placeholder={current && current.telegram_configured ? 'stored — leave blank to keep' : ''}
          value={token}
          onChange={(e) => setToken(e.target.value)}
        />
      </div>
      <div className="field">
        <label htmlFor="tgChat">Chat id</label>
        <input id="tgChat" value={chat} onChange={(e) => setChat(e.target.value)} />
      </div>
      <div className="field" style={{ flex: '0 0 8rem' }}>
        <label htmlFor="tgOn">Alerts</label>
        <select id="tgOn" value={enabled ? '1' : '0'} onChange={(e) => setEnabled(e.target.value === '1')}>
          <option value="1">on</option>
          <option value="0">off</option>
        </select>
      </div>
      <div><button type="submit" className="primary">Save</button></div>
      <p className="muted" style={{ fontSize: '.83rem', flexBasis: '100%', margin: '.4rem 0 0' }}>
        Sent when a backup fails, a disk fills, a certificate cannot be renewed
        or malware is found. The token is never shown again once saved.
      </p>
    </form>
  );
}


// Passkeys lists the account's registered authenticators.
//
// Shown only where it can work: an administrator has to switch the feature on
// for the server, and the browser has to support it. A registration button
// that fails inside the browser is worse than no button.
function Passkeys({ msg }) {
  const [state, setState] = useState(null);
  const [keys, setKeys] = useState([]);
  const [label, setLabel] = useState('');
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    try {
      const res = await api.get('/auth/passkeys');
      setState(res.state);
      setKeys(res.passkeys || []);
    } catch { /* the rest of the page still works */ }
  }, []);

  useEffect(() => { load(); }, [load]);

  if (!state) return null;

  if (!state.enabled) {
    return (
      <Card title="Passkeys">
        <p className="muted" style={{ margin: 0, fontSize: '.85rem' }}>
          Passkeys are not switched on for this server. An administrator turns
          them on under Settings.
        </p>
      </Card>
    );
  }

  if (!passkeys.supported()) {
    return (
      <Card title="Passkeys">
        <p className="muted" style={{ margin: 0, fontSize: '.85rem' }}>
          This browser cannot create passkeys here. That usually means the page
          was opened over plain HTTP or with a certificate the browser does not
          trust — passkeys only work in a secure context.
        </p>
      </Card>
    );
  }

  return (
    <Card title="Passkeys">
      <p className="muted" style={{ marginTop: 0, fontSize: '.85rem' }}>
        Used as your second step after the password, in place of a
        two-factor code. The key never leaves your phone, laptop or security
        key, and it only works at <code>{state.origin}</code> — so a copy of
        this login page on another address cannot ask for it.
      </p>

      {keys.length === 0 ? (
        <Empty>No passkeys on this account yet.</Empty>
      ) : (
        <div className="scroll">
          <table>
            <thead>
              <tr><th>Name</th><th>Added</th><th>Last used</th><th /></tr>
            </thead>
            <tbody>
              {keys.map((k) => (
                <tr key={k.id}>
                  <td>{k.label}</td>
                  <td className="muted">{fmtDate(k.created_at)}</td>
                  <td className="muted">{k.last_used_at ? fmtDate(k.last_used_at) : 'never'}</td>
                  <td className="right">
                    <button
                      type="button"
                      className="link"
                      onClick={async () => {
                        msg.clear();
                        try {
                          await api.del(`/auth/passkeys?id=${encodeURIComponent(k.id)}`);
                          msg.ok(`Removed ${k.label}`);
                          await load();
                        } catch (err) {
                          msg.fail(err);
                        }
                      }}
                    >
                      Remove
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <form
        className="row"
        style={{ marginTop: '.8rem' }}
        onSubmit={async (e) => {
          e.preventDefault();
          msg.clear();
          setBusy(true);
          try {
            const res = await passkeys.register(label.trim() || 'Passkey');
            msg.ok(`Added ${res.passkey.label}`);
            setLabel('');
            await load();
          } catch (err) {
            // A cancelled prompt is not a failure worth shouting about.
            if (err && err.name === 'NotAllowedError') msg.clear();
            else msg.fail(err);
          }
          setBusy(false);
        }}
      >
        <div className="field" style={{ flex: '0 1 16rem' }}>
          <label htmlFor="pkLabel">Name this passkey</label>
          <input
            id="pkLabel"
            placeholder="My phone"
            value={label}
            onChange={(e) => setLabel(e.target.value)}
          />
        </div>
        <div>
          <button type="submit" className="primary" disabled={busy}>
            {busy ? 'Waiting for your device…' : 'Add a passkey'}
          </button>
        </div>
      </form>
    </Card>
  );
}
