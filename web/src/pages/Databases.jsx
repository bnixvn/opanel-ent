import React, { useCallback, useEffect, useState } from 'react';
import { api, download, fmtBytes } from '../api.js';
import { Card, Copyable, Empty, Hint, Message, Search, matches, useConfirm, useMessage } from '../components.jsx';

export default function Databases({ me }) {
  const [databases, setDatabases] = useState([]);
  const [users, setUsers] = useState([]);
  const [owners, setOwners] = useState([]);
  const [query, setQuery] = useState('');
  const [secret, setSecret] = useState(null);
  const [pma, setPma] = useState(null);
  const msg = useMessage();
  const { ask, dialog } = useConfirm();

  const staff = me.role === 'admin' || me.role === 'reseller';

  const load = useCallback(async () => {
    try {
      const res = await api.get('/databases');
      setDatabases(res.databases || []);
      setUsers(res.users || []);
    } catch (err) {
      msg.fail(err);
    }
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    load();
    api.get('/phpmyadmin').then((r) => setPma(r.phpmyadmin)).catch(() => {});
    if (staff) api.get('/users').then((r) => setOwners(r.users || [])).catch(() => {});
  }, [load, staff]);

  // Which account's databases phpMyAdmin should open. Staff pick one; a
  // customer only ever has their own.
  const [pmaOwner, setPmaOwner] = useState('');

  // The link is single-use and short-lived, so it is fetched at the moment
  // the button is pressed rather than rendered into the page and left there.
  async function openPhpMyAdmin(ownerName) {
    msg.clear();
    try {
      const qs = ownerName ? `?owner=${encodeURIComponent(ownerName)}` : '';
      const res = await api.post(`/phpmyadmin/signon${qs}`, {});
      window.open(res.url, '_blank', 'noopener');
    } catch (err) {
      msg.fail(err);
    }
  }

  async function guard(fn, okText) {
    msg.clear();
    try {
      const res = await fn();
      msg.ok(okText);
      return res;
    } catch (err) {
      msg.fail(err);
      return null;
    } finally {
      load();
    }
  }

  const shownDatabases = databases.filter((d) => matches(d.name, query) || matches(d.owner, query));
  const shownUsers = users.filter((u) => matches(u.username, query) || matches(u.owner, query));

  return (
    <>
      {dialog}
      <Message value={msg.message} onClear={msg.clear} />

      <Card
        title={(
          <>
            phpMyAdmin
            <Hint>
              Opens with a throwaway database account that can reach only the
              chosen owner&apos;s databases and expires by itself. The panel
              never stores your database password, so it cannot sign in as you.
            </Hint>
          </>
        )}
      >
        {!pma ? null : pma.installed ? (
          <div className="row" style={{ alignItems: 'flex-end' }}>
            {staff && (
              <div className="field" style={{ flex: '0 0 14rem' }}>
                <label htmlFor="pmaOwner">Account</label>
                <select
                  id="pmaOwner"
                  value={pmaOwner}
                  onChange={(e) => setPmaOwner(e.target.value)}
                >
                  <option value="">{me.username} (me)</option>
                  {owners.filter((u) => u.linux_uid).map((u) => (
                    <option key={u.id} value={u.username}>{u.username}</option>
                  ))}
                </select>
              </div>
            )}
            <div>
              <button
                type="button"
                className="primary"
                onClick={() => openPhpMyAdmin(pmaOwner)}
              >
                Open phpMyAdmin
              </button>
            </div>
          </div>
        ) : (
          <div className="row" style={{ alignItems: 'center' }}>
            <div>
              <button
                type="button"
                className="primary"
                onClick={() => guard(() => api.post('/phpmyadmin/install', {}), 'phpMyAdmin installed')
                  .then(() => api.get('/phpmyadmin').then((r) => setPma(r.phpmyadmin)).catch(() => {}))}
              >
                Install phpMyAdmin
              </button>
            </div>
            <p className="muted" style={{ flex: 1, margin: 0, fontSize: '.83rem' }}>
              Not installed yet.
            </p>
          </div>
        )}
      </Card>

      <Card title="New database">
        <NewDatabase staff={staff} owners={owners} me={me} onCreated={setSecret} onDone={guard} />
      </Card>

      {secret && (
        <Card title={secret.database ? 'Database created' : 'New password'}>
          <p className="muted" style={{ marginTop: 0, fontSize: '.85rem' }}>
            The password is shown once. Put it into the application&apos;s
            configuration now.
          </p>
          {secret.warning && <Message value={{ kind: 'warn', text: secret.warning }} />}
          <div className="grid2">
            <Copyable label="Database" value={secret.database} />
            <Copyable label="Username" value={secret.username} />
            <Copyable label="Host" value={secret.host || 'localhost'} />
            <Copyable label="Password" value={secret.password} />
          </div>
          <p style={{ marginBottom: 0 }}>
            <button type="button" onClick={() => setSecret(null)}>Close</button>
          </p>
        </Card>
      )}

      <Card
        title="Databases"
        actions={<Search value={query} onChange={setQuery} placeholder="Search…" />}
      >
        {shownDatabases.length === 0 ? (
          <Empty>{databases.length ? 'Nothing matches that search.' : 'No databases yet.'}</Empty>
        ) : (
          <div className="scroll">
            <table>
              <thead>
                <tr>
                  <th>Name</th>
                  {staff && <th>Owner</th>}
                  <th>Accounts</th>
                  <th>Size</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {shownDatabases.map((d) => (
                  <tr key={d.id}>
                    <td><code>{d.name}</code></td>
                    {staff && <td>{d.owner}</td>}
                    <td className="muted">{(d.users || []).join(', ') || '—'}</td>
                    <td className="muted">{fmtBytes(d.size_bytes)}</td>
                    <td className="right nowrap">
                      <button
                        type="button"
                        onClick={() => download(`/databases/${d.id}/export`)}
                      >
                        Download
                      </button>{' '}
                      <button
                        type="button"
                        className="danger"
                        onClick={async () => {
                          const ok = await ask({
                            title: `Delete ${d.name}?`,
                            body: 'Every table in it goes with it. This cannot be undone.',
                            confirmLabel: 'Delete',
                            danger: true,
                          });
                          if (ok) guard(() => api.del(`/databases/${d.id}`), 'Database deleted');
                        }}
                      >
                        Delete
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>

      <Card title="Database accounts">
        {shownUsers.length === 0 ? (
          <Empty>No accounts yet.</Empty>
        ) : (
          <div className="scroll">
            <table>
              <thead>
                <tr>
                  <th>Username</th>
                  {staff && <th>Owner</th>}
                  <th>Can reach</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {shownUsers.map((u) => (
                  <tr key={u.id}>
                    <td><code>{u.username}</code></td>
                    {staff && <td>{u.owner}</td>}
                    <td className="muted">{(u.databases || []).join(', ') || '—'}</td>
                    <td className="right nowrap">
                      <button
                        type="button"
                        onClick={async () => {
                          const res = await guard(
                            () => api.post(`/database-users/${u.id}/password`, {}),
                            'Password reset',
                          );
                          if (res && res.password) {
                            setSecret({ username: u.username, host: 'localhost', password: res.password });
                          }
                        }}
                      >
                        Reset password
                      </button>{' '}
                      <button
                        type="button"
                        className="danger"
                        onClick={async () => {
                          const ok = await ask({
                            title: `Delete ${u.username}?`,
                            body: 'Any application still using it will stop being able to connect.',
                            confirmLabel: 'Delete',
                            danger: true,
                          });
                          if (ok) guard(() => api.del(`/database-users/${u.id}`), 'Account deleted');
                        }}
                      >
                        Delete
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>
    </>
  );
}

function NewDatabase({ staff, owners, me, onCreated, onDone }) {
  const [name, setName] = useState('');
  const [user, setUser] = useState('');
  const [password, setPassword] = useState('');
  const [owner, setOwner] = useState('');

  // The owner's name is prefixed by the server, so the field shows what will
  // actually be created rather than the half of it the form collects.
  const prefix = `${staffOwnerName(staff, owners, owner, me)}_`;

  return (
    <form
      onSubmit={async (e) => {
        e.preventDefault();
        const body = { suffix: name.trim() };
        if (user.trim()) body.user = user.trim();
        if (password) body.password = password;
        if (staff && owner) body.owner_id = Number(owner);
        const res = await onDone(() => api.post('/databases', body), `Created ${name.trim()}`);
        if (res) {
          setName('');
          setUser('');
          setPassword('');
          onCreated({
            database: res.database ? res.database.name : res.name,
            username: res.user ? res.user.username : '',
            password: res.password || '',
            host: res.host,
            warning: res.warning || '',
          });
        }
      }}
    >
      <div className="row">
        <div className="field">
          <label htmlFor="ndName">Database name</label>
          <Prefixed prefix={prefix}>
            <input
              id="ndName"
              placeholder="shop"
              required
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
          </Prefixed>
        </div>
        <div className="field">
          <label htmlFor="ndUser">
            Database user
            <Hint>Left empty, the account takes the database&apos;s own name.</Hint>
          </label>
          <Prefixed prefix={prefix}>
            <input
              id="ndUser"
              placeholder={name || 'shop'}
              value={user}
              onChange={(e) => setUser(e.target.value)}
            />
          </Prefixed>
        </div>
        {staff && (
          <div className="field" style={{ flex: '0 0 11rem' }}>
            <label htmlFor="ndOwner">Owner</label>
            <select id="ndOwner" value={owner} onChange={(e) => setOwner(e.target.value)}>
              <option value="">{me.username} (me)</option>
              {owners.filter((u) => u.linux_uid).map((u) => (
                <option key={u.id} value={u.id}>{u.username}</option>
              ))}
            </select>
          </div>
        )}
      </div>
      <div className="row">
        <div className="field">
          <label htmlFor="ndPass">
            Password
            <Hint>
              12 to 128 characters: letters, digits, underscore or hyphen.
              Left empty, the panel invents one.
            </Hint>
          </label>
          <div className="withbutton">
            <input
              id="ndPass"
              type="text"
              placeholder="generated for you"
              autoComplete="new-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
            <button type="button" onClick={() => setPassword(generatePassword())}>
              Generate
            </button>
          </div>
        </div>
        <div>
          <button type="submit" className="primary">Create database</button>
        </div>
      </div>
    </form>
  );
}

// Prefixed shows the owner prefix the server will add, attached to the front
// of the field so the name in the box is the name that gets created.
function Prefixed({ prefix, children }) {
  return (
    <div className="prefixed">
      <span>{prefix}</span>
      {children}
    </div>
  );
}

function staffOwnerName(staff, owners, ownerID, me) {
  if (!staff || !ownerID) return me.username;
  const found = owners.find((u) => String(u.id) === String(ownerID));
  return found ? found.username : me.username;
}

// generatePassword draws from the alphabet the database server accepts.
// crypto.getRandomValues rather than Math.random: this is a credential, and
// the modulo bias of a 62-character alphabet over 256 is small enough not to
// matter beside the difference between a CSPRNG and a seeded one.
function generatePassword() {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789';
  const bytes = new Uint8Array(20);
  crypto.getRandomValues(bytes);
  return Array.from(bytes, (b) => alphabet[b % alphabet.length]).join('');
}
