import React, { useCallback, useEffect, useState } from 'react';
import { api } from '../api.js';
import { Card, Empty, Message, Tag, useConfirm, useMessage } from '../components.jsx';

export default function Webserver() {
  const [state, setState] = useState(null);
  const [busy, setBusy] = useState(null);
  const [serial, setSerial] = useState('');
  const msg = useMessage();
  const { ask, dialog } = useConfirm();

  const load = useCallback(() => {
    api.get('/webserver/backends').then(setState).catch(msg.fail);
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => { load(); }, [load]);

  async function switchTo(b) {
    const ok = await ask({
      title: `Switch to ${b.label}?`,
      // The one thing a person needs to know before pressing it, and the one
      // thing they will be angry about not being told.
      body: 'Every site is re-rendered for the new server and the old one is'
        + ' stopped. Sites are unreachable for a few seconds during the'
        + ' handover. If the new server fails to start, the old one is put'
        + ' back and nothing changes.',
      confirmLabel: 'Switch',
    });
    if (!ok) return;
    msg.clear();
    setBusy(b.name);
    try {
      const r = await api.post('/webserver/switch', { backend: b.name });
      msg.ok(`Now serving with ${r.backend}, PHP through ${r.php_provider}.`);
    } catch (err) {
      msg.fail(err);
    }
    setBusy(null);
    load();
  }

  async function installLiteSpeed() {
    msg.clear();
    setBusy('install');
    try {
      const r = await api.post('/webserver/litespeed/install', { serial: serial.trim() });
      msg.ok(r.licence === 'trial'
        ? `LiteSpeed ${r.version} installed on a trial licence, valid to ${r.expires}.`
        : `LiteSpeed ${r.version} installed.`);
      setSerial('');
    } catch (err) {
      msg.fail(err);
    }
    setBusy(null);
    load();
  }

  const lsws = state?.litespeed || {};

  return (
    <>
      <Message value={msg.message} onClear={msg.clear} />
      {dialog}

      <Card title="Web server">
        {!state ? <Empty>Loading.</Empty> : (
          <div className="scroll">
            <table>
              <thead>
                <tr><th>Server</th><th>State</th><th>Unit</th><th /></tr>
              </thead>
              <tbody>
                {state.backends.map((b) => (
                  <tr key={b.name}>
                    <td>
                      <strong>{b.label || b.name}</strong>
                      <div className="muted" style={{ fontSize: '.85em' }}>{b.note}</div>
                    </td>
                    <td className="nowrap">
                      {b.active ? <Tag kind="ok">serving</Tag>
                        : b.installed ? <Tag>installed</Tag>
                          : <Tag kind="bad">not installed</Tag>}
                    </td>
                    <td className="muted"><code>{b.unit}</code></td>
                    <td className="right nowrap">
                      {!b.active && b.installed && (
                        <button
                          type="button"
                          disabled={busy !== null}
                          onClick={() => switchTo(b)}
                        >
                          {busy === b.name ? 'Switching…' : 'Switch'}
                        </button>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>

      {state && (
        <Card title="LiteSpeed Enterprise">
          {lsws.installed ? (
            <dl className="kv">
              <dt>Version</dt><dd>{lsws.version || '—'}</dd>
              <dt>Licence</dt>
              <dd>
                {lsws.licence === 'trial'
                  ? <>trial{lsws.expires && <> · expires {lsws.expires}</>}</>
                  : lsws.licence || '—'}
              </dd>
              <dt>Running</dt><dd>{lsws.running ? 'yes' : 'no'}</dd>
            </dl>
          ) : (
            <>
              <p className="muted">
                Reads the same configuration Apache does, so switching to it
                moves no files and switching back is just as quick.
              </p>
              <div className="row">
                <input
                  type="text"
                  value={serial}
                  placeholder="Serial number, or leave empty for a 15-day trial"
                  onChange={(e) => setSerial(e.target.value)}
                  style={{ minWidth: '22rem' }}
                />
                <button type="button" disabled={busy !== null} onClick={installLiteSpeed}>
                  {busy === 'install' ? 'Installing…' : 'Install'}
                </button>
              </div>
            </>
          )}
        </Card>
      )}

      {state && (
        <Card title="PHP">
          <p className="muted">
            A version runs only while a site uses it. One with no sites is
            stopped on purpose: an idle pool manager is memory spent serving
            nobody, and it starts again the moment a site is pointed at it.
          </p>
          <div className="scroll">
            <table>
              <thead>
                <tr><th>Version</th><th>Sites</th><th>State</th><th>Interpreter</th></tr>
              </thead>
              <tbody>
                {(state.php_versions || []).filter((v) => v.installed).map((v) => (
                  <tr key={v.version}>
                    <td><strong>{v.version}</strong></td>
                    <td>{v.pools || 0}</td>
                    <td className="nowrap">
                      {v.running ? <Tag kind="ok">running</Tag>
                        : v.pools ? <Tag kind="bad">stopped</Tag>
                          : <Tag>idle</Tag>}
                    </td>
                    <td className="muted"><code>{v.fpm_path || '—'}</code></td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <p className="muted" style={{ marginTop: '.75rem' }}>
            Provider <code>{state.php_provider}</code>.
          </p>
        </Card>
      )}
    </>
  );
}
