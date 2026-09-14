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

  const check = useCallback(() => {
    api.get('/terminal/status').then(setStatus).catch(() => setStatus({ available: false }));
  }, []);

  useEffect(() => { check(); }, [check]);

  if (!status) return null;
  if (!status.available) {
    return (
      <Card title="Terminal">
        <EnableAccess reason={status.reason} canEnable={status.can_enable} onEnabled={check} />
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
        <>
          <p className="muted" style={{ margin: 0, fontSize: '.85rem' }}>
            Runs as <code>{status.username}</code>, in that account&apos;s home
            directory.
          </p>
          {status.commands && (
            <>
              <h3 className="subhead">
                Commands
                <Hint>
                  Anything else is refused. It keeps the rest of the system out
                  of reach of a mistake; it is not a sandbox, because php, node
                  and git run whatever they are given.
                </Hint>
              </h3>
              <div className="cmdlist">
                {status.commands.map((c) => <code key={c}>{c}</code>)}
              </div>
            </>
          )}
        </>
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
        <EnableAccess
          reason={data.unavailable}
          canEnable
          onEnabled={load}
          onCredentials={setMade}
        />
        {made && <NewCredentials made={made} onClose={() => setMade(null)} />}
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
      <p style={{ margin: '.2rem 0 0' }}>
        <button
          type="button"
          onClick={async () => {
            msg.clear();
            try {
              setMade(await api.post('/sftp/primary/password', {}));
            } catch (err) {
              msg.fail(err);
            }
          }}
        >
          Reset this password
        </button>
      </p>

      <h3 className="subhead">
        Extra logins
        <Hint>
          A username and a password of its own -- a second way in, not a
          second password for the one above. As many as you need, up to
          {` ${data.limit || 20}`}. Each has the account&apos;s own
          permissions: the same files, read and write, confined to this home
          directory and nothing outside it. Removing one leaves the others
          alone. None of them can open a shell, and none can be limited to a
          single website.
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

      {made && <NewCredentials made={made} onClose={() => setMade(null)} />}

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
              {busy ? 'Creating…' : 'Add a login'}
            </button>
          </div>
        </form>
      )}
    </Card>
  );
}

function NewCredentials({ made, onClose }) {
  return (
    <div className="madecreds">
      <div className="grid2">
        <Copyable label="Host" value={made.host} />
        <Copyable label="Port" value={String(made.port || 22)} />
        <Copyable
          label="Username"
          value={made.username || (made.account && made.account.username)}
        />
        <Copyable label="Password" value={made.password} />
      </div>
      <p className="muted" style={{ margin: '.2rem 0 .6rem', fontSize: '.82rem' }}>
        The password is shown once.
      </p>
      <button type="button" onClick={onClose}>Close</button>
    </div>
  );
}

// EnableAccess offers an account with no Linux user one.
//
// Administrators and resellers start without one because they own no
// websites, which leaves them no shell to run curl in and nowhere to put a
// backup they fetched. This makes one on request: an ordinary unprivileged
// account with a home of its own, not root.
function EnableAccess({ reason, canEnable, onEnabled, onCredentials }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');

  if (!canEnable) return <Empty>{reason}</Empty>;

  return (
    <>
      <Empty>{reason}</Empty>
      {error && <div className="msg err">{error}</div>}
      <p style={{ textAlign: 'center', margin: '.6rem 0 0' }}>
        <button
          type="button"
          className="primary"
          disabled={busy}
          onClick={async () => {
            setBusy(true);
            setError('');
            try {
              const res = await api.post('/account/file-access', {});
              if (onCredentials) onCredentials(res);
              if (onEnabled) onEnabled();
            } catch (err) {
              setError(err && err.message ? err.message : String(err));
            }
            setBusy(false);
          }}
        >
          {busy ? 'Setting up…' : 'Add a Linux user for this account'}
        </button>
      </p>
    </>
  );
}
