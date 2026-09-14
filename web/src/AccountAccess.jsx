import React, { Suspense, useCallback, useEffect, useState } from 'react';
import { api, fmtDate } from './api.js';
import { Card, Copyable, Empty, Hint, Message, useConfirm, useMessage } from './components.jsx';

// xterm is a quarter of a megabyte and most visits never open a shell, so it
// arrives on its own chunk the first time somebody asks for one.
const Terminal = React.lazy(() => import('./Terminal.jsx'));

// AccountAccess is the two ways into an account that are not the panel: a
// shell, and the credentials for an SFTP client.

export function TerminalCard() {
  const [status, setStatus] = useState(null);
  const [open, setOpen] = useState(false);

  useEffect(() => {
    api.get('/terminal/status').then(setStatus).catch(() => setStatus({ available: false }));
  }, []);

  if (!status) return null;
  if (!status.available) {
    return (
      <Card title="Terminal">
        <Empty>{status.reason || 'No shell is available for this account.'}</Empty>
      </Card>
    );
  }

  return (
    <Card
      title="Terminal"
      actions={!open && (
        <button type="button" className="primary" onClick={() => setOpen(true)}>
          Open a shell
        </button>
      )}
    >
      {open ? (
        <Suspense fallback={<p className="muted">Loading the terminal…</p>}>
          <Terminal username={status.username} onClose={() => setOpen(false)} />
        </Suspense>
      ) : (
        <p className="muted" style={{ margin: 0, fontSize: '.85rem' }}>
          Runs as <code>{status.username}</code>, in that account&apos;s home directory.
        </p>
      )}
    </Card>
  );
}

export function SFTPCard() {
  const [data, setData] = useState(null);
  const [label, setLabel] = useState('');
  const [note, setNote] = useState('');
  const [busy, setBusy] = useState(false);
  const [made, setMade] = useState(null);
  const msg = useMessage();
  const { ask, dialog } = useConfirm();

  const load = useCallback(async () => {
    try {
      setData(await api.get('/sftp'));
    } catch (err) {
      // An account with no Linux user has nothing here; that is a state, not
      // a failure, and the card says so rather than showing an error.
      setData({ unavailable: err && err.message ? err.message : 'not available' });
    }
  }, []);

  useEffect(() => { load(); }, [load]);

  if (!data) return null;
  if (data.unavailable) {
    return (
      <Card title="SFTP">
        <Empty>{data.unavailable}</Empty>
      </Card>
    );
  }

  const rows = data.rows || [];
  const full = rows.length >= (data.limit || 20);

  return (
    <Card title="SFTP">
      {dialog}
      <Message value={msg.message} onClear={msg.clear} />

      <div className="grid2">
        <Copyable label="Host" value={data.host} />
        <Copyable label="Port" value={String(data.port || 22)} />
        <Copyable label="Username" value={data.primary} hint="The account's own login. It cannot be removed." />
      </div>

      <h3 className="subhead">
        Extra credentials
        <Hint>
          Each one is a separate login with its own password that can be
          withdrawn on its own. They all reach the same files.
        </Hint>
      </h3>

      {rows.length === 0 ? (
        <Empty>None yet.</Empty>
      ) : (
        <div className="scroll">
          <table>
            <thead>
              <tr><th>Username</th><th>For</th><th>Added</th><th /></tr>
            </thead>
            <tbody>
              {rows.map((a) => (
                <tr key={a.id}>
                  <td><code>{a.username}</code></td>
                  <td className="muted">{a.note || '—'}</td>
                  <td className="muted nowrap">{fmtDate(a.created_at)}</td>
                  <td className="right nowrap">
                    <button
                      type="button"
                      className="link"
                      onClick={async () => {
                        msg.clear();
                        try {
                          const res = await api.post(`/sftp/${a.id}/password`, {});
                          setMade(res);
                        } catch (err) {
                          msg.fail(err);
                        }
                        await load();
                      }}
                    >
                      Reset password
                    </button>
                    <button
                      type="button"
                      className="link danger"
                      onClick={async () => {
                        const ok = await ask({
                          title: `Remove ${a.username}?`,
                          body: 'That login stops working immediately. The files it '
                            + 'could reach belong to the account and are not touched.',
                          confirmLabel: 'Remove',
                          danger: true,
                        });
                        if (!ok) return;
                        try {
                          await api.del(`/sftp/${a.id}`);
                          msg.ok(`${a.username} removed.`);
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

      {made && (
        <div className="madecreds">
          <div className="grid2">
            <Copyable label="Host" value={made.host} />
            <Copyable label="Port" value={String(made.port || 22)} />
            <Copyable label="Username" value={made.username || (made.account && made.account.username)} />
            <Copyable label="Password" value={made.password} />
          </div>
          <p className="muted" style={{ margin: '.2rem 0 .6rem', fontSize: '.82rem' }}>
            The password is shown once.
          </p>
          <button type="button" onClick={() => setMade(null)}>Close</button>
        </div>
      )}

      {!full && (
        <form
          className="row"
          style={{ marginTop: '1rem' }}
          onSubmit={async (e) => {
            e.preventDefault();
            msg.clear();
            setBusy(true);
            try {
              const res = await api.post('/sftp', { label: label.trim(), note: note.trim() });
              setMade(res);
              setLabel('');
              setNote('');
            } catch (err) {
              msg.fail(err);
            }
            setBusy(false);
            await load();
          }}
        >
          <div className="field">
            <label htmlFor="sftpLabel">
              Name
              <Hint>
                Lowercase letters and digits. The login becomes
                {` ${data.primary}_`}
                followed by this.
              </Hint>
            </label>
            <div className="prefixed">
              <span>{data.primary}_</span>
              <input
                id="sftpLabel"
                required
                placeholder="deploy"
                value={label}
                onChange={(e) => setLabel(e.target.value)}
              />
            </div>
          </div>
          <div className="field">
            <label htmlFor="sftpNote">For</label>
            <input
              id="sftpNote"
              placeholder="the agency doing the redesign"
              value={note}
              onChange={(e) => setNote(e.target.value)}
            />
          </div>
          <div>
            <button type="submit" className="primary" disabled={busy}>
              {busy ? 'Creating…' : 'Add a credential'}
            </button>
          </div>
        </form>
      )}
    </Card>
  );
}
