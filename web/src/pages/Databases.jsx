import React, { useCallback, useEffect, useState } from 'react';
import { api, download, fmtBytes } from '../api.js';
import { Card, Empty, Message, Search, Secret, matches, useConfirm, useMessage } from '../components.jsx';

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

      <Card title="phpMyAdmin">
        {!pma ? null : pma.installed ? (
          <div className="row" style={{ alignItems: 'center' }}>
            <div>
              <button
                type="button"
                className="primary"
                onClick={() => openPhpMyAdmin('')}
              >
                Open phpMyAdmin
              </button>
            </div>
            {staff && owners.filter((u) => u.linux_uid).length > 0 && (
              <div className="field" style={{ flex: '0 0 12rem' }}>
                <label htmlFor="pmaOwner">as</label>
                <select
                  id="pmaOwner"
                  onChange={(e) => e.target.value && openPhpMyAdmin(e.target.value)}
                  defaultValue=""
                >
                  <option value="">pick an account…</option>
                  {owners.filter((u) => u.linux_uid).map((u) => (
                    <option key={u.id} value={u.username}>{u.username}</option>
                  ))}
                </select>
              </div>
            )}
            <p className="muted" style={{ flex: 1, fontSize: '.83rem', margin: 0 }}>
              Opens with a throwaway database account that can reach only this
              owner&apos;s databases and expires by itself. The panel never stores
              your database password, so it cannot sign in as you.
            </p>
          </div>
        ) : (
          <>
            <p className="muted" style={{ marginTop: 0, fontSize: '.85rem' }}>
              Not installed. It is served through the panel on the loopback
              address, so it needs no hostname, no certificate and no open port.
            </p>
            <button
              type="button"
              onClick={() => guard(() => api.post('/phpmyadmin/install', {}), 'phpMyAdmin installed')
                .then(() => api.get('/phpmyadmin').then((r) => setPma(r.phpmyadmin)).catch(() => {}))}
            >
              Install phpMyAdmin
            </button>
          </>
        )}
      </Card>

      <Card title="New database">
        <NewDatabase staff={staff} owners={owners} me={me} onCreated={setSecret} onDone={guard} />
      </Card>

      {secret && (
        <Card title="Database created">
          <p className="muted" style={{ marginTop: 0, fontSize: '.85rem' }}>
            The password is shown once. Put it into the application's
            configuration now.
          </p>
          <dl className="kv">
            <dt>Database</dt><dd><code>{secret.database}</code></dd>
            <dt>Username</dt><dd><code>{secret.username}</code></dd>
            <dt>Host</dt><dd><code>{secret.host || '127.0.0.1'}</code></dd>
          </dl>
          <Secret label="Password" value={secret.password} />
          <button type="button" onClick={() => setSecret(null)}>Close</button>
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
                            setSecret({ database: '—', username: u.username, password: res.password });
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
  const [owner, setOwner] = useState('');

  return (
    <form
      className="row"
      onSubmit={async (e) => {
        e.preventDefault();
        const body = { suffix: name.trim() };
        if (staff && owner) body.owner_id = Number(owner);
        const res = await onDone(() => api.post('/databases', body), `Created ${name.trim()}`);
        setName('');
        if (res) {
          onCreated({
            database: res.database ? res.database.name : res.name,
            username: res.user ? res.user.username : '',
            password: res.password || '',
            host: res.host,
          });
        }
      }}
    >
      <div className="field">
        <label htmlFor="ndName">Name</label>
        <input
          id="ndName"
          placeholder="shop"
          required
          value={name}
          onChange={(e) => setName(e.target.value)}
        />
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
      <div>
        <button type="submit" className="primary">Create database and account</button>
      </div>
      <p className="muted" style={{ fontSize: '.83rem', flexBasis: '100%', margin: '.4rem 0 0' }}>
        Your username is added as a prefix, so <code>shop</code> becomes{' '}
        <code>{me.username}_shop</code>. A matching account with access to it is
        created at the same time.
      </p>
    </form>
  );
}
