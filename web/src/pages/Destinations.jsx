import React, { useCallback, useEffect, useState } from 'react';
import { api, fmtDate } from '../api.js';
import { Card, Empty, Message, Tag, useConfirm, useMessage } from '../components.jsx';

// Destinations is where backups are copied to, besides this server.
//
// It lives on the Backups page because that is the only reason it exists: a
// backup that sits on the machine it was taken from is not a backup of that
// machine.
export default function Destinations({ me }) {
  const [rows, setRows] = useState([]);
  const [adding, setAdding] = useState(false);
  const [busy, setBusy] = useState(null);
  const msg = useMessage();
  const { ask, dialog } = useConfirm();

  const admin = me.role === 'admin';

  const load = useCallback(async () => {
    try {
      const res = await api.get('/backup-destinations');
      setRows(res.destinations || []);
    } catch (err) {
      msg.fail(err);
    }
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => { load(); }, [load]);

  return (
    <Card
      title="Where backups are copied"
      actions={!adding && (
        <button type="button" onClick={() => setAdding(true)}>Add a destination</button>
      )}
    >
      {dialog}
      <Message value={msg.message} onClear={msg.clear} />

      <p className="muted" style={{ marginTop: 0, fontSize: '.85rem' }}>
        Every finished backup is copied to these. A backup that only exists on
        this server is not a backup of this server. The panel stores the
        address and never the password — that lives in a root-only file, so a
        copy of the panel&apos;s database is not a copy of your storage
        credentials.
      </p>

      {rows.length === 0 ? (
        <Empty>Backups are kept on this server only.</Empty>
      ) : (
        <div className="scroll">
          <table>
            <thead>
              <tr><th>Name</th><th>Where</th><th>Status</th><th /></tr>
            </thead>
            <tbody>
              {rows.map((d) => (
                <tr key={d.id}>
                  <td>
                    {d.name}{' '}
                    {d.server ? <Tag kind="mute">whole server</Tag> : <Tag>{d.owner}</Tag>}
                  </td>
                  <td className="muted" style={{ fontSize: '.82rem', wordBreak: 'break-all' }}>
                    <code>{d.kind}</code> {d.summary}
                  </td>
                  <td className="nowrap">
                    {!d.enabled ? <Tag kind="mute">off</Tag>
                      : d.last_error ? <Tag kind="bad">failing</Tag>
                        : d.last_ok_at && !d.last_ok_at.startsWith('0001')
                          ? <Tag kind="ok">ok</Tag> : <Tag kind="warn">untested</Tag>}
                    {d.last_error && (
                      <div className="muted" style={{ fontSize: '.75rem', maxWidth: '18rem' }}>
                        {d.last_error}
                      </div>
                    )}
                    {!d.last_error && d.last_ok_at && !d.last_ok_at.startsWith('0001') && (
                      <div className="muted" style={{ fontSize: '.75rem' }}>
                        {fmtDate(d.last_ok_at)}
                      </div>
                    )}
                  </td>
                  <td className="right nowrap">
                    <button
                      type="button"
                      className="link"
                      disabled={busy === d.id}
                      onClick={async () => {
                        msg.clear();
                        setBusy(d.id);
                        try {
                          const res = await api.post(`/backup-destinations/${d.id}/test`, {});
                          msg.ok(res.message || 'Connected.');
                        } catch (err) {
                          msg.fail(err);
                        }
                        setBusy(null);
                        await load();
                      }}
                    >
                      {busy === d.id ? 'Testing…' : 'Test'}
                    </button>{' '}
                    <button
                      type="button"
                      className="link"
                      onClick={async () => {
                        const ok = await ask({
                          title: `Remove ${d.name}?`,
                          body: 'New backups stop being copied there. Archives already '
                            + 'on it are left alone — the panel does not delete them.',
                          confirmLabel: 'Remove',
                          danger: true,
                        });
                        if (!ok) return;
                        try {
                          await api.del(`/backup-destinations/${d.id}`);
                          msg.ok('Destination removed.');
                        } catch (err) {
                          msg.fail(err);
                        }
                        await load();
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

      {adding && (
        <DestinationForm
          admin={admin}
          onCancel={() => setAdding(false)}
          onSaved={(message) => { setAdding(false); msg.ok(message); load(); }}
          onFailed={(err) => msg.fail(err)}
        />
      )}
    </Card>
  );
}

function DestinationForm({ admin, onSaved, onCancel, onFailed }) {
  const [kind, setKind] = useState('sftp');
  const [form, setForm] = useState({
    name: '', host: '', port: 22, user: '', path: '', host_key: '',
    secret_kind: 'password', secret: '',
    bucket: '', region: '', endpoint: '', access_key: '',
    server: false,
  });
  const [busy, setBusy] = useState(false);
  const set = (k) => (e) => setForm({ ...form, [k]: e.target.value });

  return (
    <form
      style={{ marginTop: '1rem', borderTop: '1px solid var(--line)', paddingTop: '.8rem' }}
      onSubmit={async (e) => {
        e.preventDefault();
        setBusy(true);
        try {
          const res = await api.post('/backup-destinations', { ...form, kind });
          onSaved(res.message || 'Destination added and tested.');
        } catch (err) {
          onFailed(err);
        }
        setBusy(false);
      }}
    >
      <div className="row">
        <div className="field" style={{ flex: '0 0 8rem' }}>
          <label htmlFor="dKind">Type</label>
          <select id="dKind" value={kind} onChange={(e) => setKind(e.target.value)}>
            <option value="sftp">SFTP</option>
            <option value="s3">S3</option>
          </select>
        </div>
        <div className="field">
          <label htmlFor="dName">Name</label>
          <input id="dName" required placeholder="offsite box" value={form.name} onChange={set('name')} />
        </div>
        {admin && (
          <div className="field" style={{ flex: '0 0 12rem', alignSelf: 'flex-end' }}>
            <label className="check">
              <input
                type="checkbox"
                checked={form.server}
                onChange={(e) => setForm({ ...form, server: e.target.checked })}
              />
              For every account
            </label>
          </div>
        )}
      </div>

      {kind === 'sftp' ? (
        <>
          <div className="row">
            <div className="field"><label htmlFor="dHost">Host</label>
              <input id="dHost" required placeholder="backup.example.com" value={form.host} onChange={set('host')} />
            </div>
            <div className="field" style={{ flex: '0 0 6rem' }}><label htmlFor="dPort">Port</label>
              <input id="dPort" type="number" value={form.port} onChange={set('port')} />
            </div>
            <div className="field" style={{ flex: '0 0 10rem' }}><label htmlFor="dUser">Username</label>
              <input id="dUser" required value={form.user} onChange={set('user')} />
            </div>
            <div className="field"><label htmlFor="dPath">Directory</label>
              <input id="dPath" placeholder="/backups/opanel" value={form.path} onChange={set('path')} />
            </div>
          </div>
          <div className="field">
            <label htmlFor="dHostKey">The server&apos;s host key</label>
            <input
              id="dHostKey"
              required
              placeholder="ssh-ed25519 AAAAC3Nza…"
              value={form.host_key}
              onChange={set('host_key')}
              style={{ fontFamily: 'ui-monospace, monospace', fontSize: '.8rem' }}
            />
            <div className="muted" style={{ fontSize: '.78rem' }}>
              Run <code>ssh-keyscan -t ed25519 your-host</code> and paste
              everything after the address. Without it the panel would hand
              every customer&apos;s files to whoever answers that address.
            </div>
          </div>
          <div className="row">
            <div className="field" style={{ flex: '0 0 12rem' }}>
              <label htmlFor="dSecretKind">Sign in with</label>
              <select id="dSecretKind" value={form.secret_kind} onChange={set('secret_kind')}>
                <option value="password">A password</option>
                <option value="key">A private key</option>
              </select>
            </div>
            <div className="field">
              <label htmlFor="dSecret">
                {form.secret_kind === 'key' ? 'Private key' : 'Password'}
              </label>
              {form.secret_kind === 'key' ? (
                <textarea
                  id="dSecret"
                  required
                  rows={4}
                  placeholder="-----BEGIN OPENSSH PRIVATE KEY-----"
                  value={form.secret}
                  onChange={set('secret')}
                  style={{ fontFamily: 'ui-monospace, monospace', fontSize: '.78rem' }}
                />
              ) : (
                <input id="dSecret" type="password" required value={form.secret} onChange={set('secret')} />
              )}
              <div className="muted" style={{ fontSize: '.78rem' }}>
                Stored on this server only, readable by root. An encrypted key
                needs its passphrase removed first.
              </div>
            </div>
          </div>
        </>
      ) : (
        <>
          <div className="row">
            <div className="field"><label htmlFor="dBucket">Bucket</label>
              <input id="dBucket" required value={form.bucket} onChange={set('bucket')} />
            </div>
            <div className="field" style={{ flex: '0 0 9rem' }}><label htmlFor="dRegion">Region</label>
              <input id="dRegion" placeholder="us-east-1" value={form.region} onChange={set('region')} />
            </div>
            <div className="field"><label htmlFor="dEndpoint">Endpoint</label>
              <input id="dEndpoint" placeholder="s3.amazonaws.com" value={form.endpoint} onChange={set('endpoint')} />
              <div className="muted" style={{ fontSize: '.78rem' }}>
                Leave empty for Amazon. Wasabi, Backblaze, R2 and MinIO all work.
              </div>
            </div>
          </div>
          <div className="row">
            <div className="field"><label htmlFor="dAccess">Access key</label>
              <input id="dAccess" required value={form.access_key} onChange={set('access_key')} />
            </div>
            <div className="field"><label htmlFor="dSecretKey">Secret key</label>
              <input id="dSecretKey" type="password" required value={form.secret} onChange={set('secret')} />
            </div>
            <div className="field"><label htmlFor="dPrefix">Prefix</label>
              <input id="dPrefix" placeholder="opanel/" value={form.path} onChange={set('path')} />
            </div>
          </div>
        </>
      )}

      <p style={{ marginBottom: 0 }}>
        <button type="submit" className="primary" disabled={busy}>
          {busy ? 'Connecting…' : 'Add and test'}
        </button>{' '}
        <button type="button" onClick={onCancel} disabled={busy}>Cancel</button>{' '}
        <span className="muted" style={{ fontSize: '.8rem' }}>
          The panel writes and removes a test file before keeping this, so a
          destination that was never reachable is never saved.
        </span>
      </p>
    </form>
  );
}
