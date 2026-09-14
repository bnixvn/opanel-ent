import React, { useCallback, useEffect, useState } from 'react';
import { api, fmtDate } from '../api.js';
import { Card, Empty, Message, Tag, useMessage } from '../components.jsx';

export default function System() {
  const [info, setInfo] = useState(null);
  const [units, setUnits] = useState([]);
  const [audit, setAudit] = useState([]);
  const [busy, setBusy] = useState(null);
  const msg = useMessage();

  const load = useCallback(async () => {
    api.get('/system/info').then(setInfo).catch(msg.fail);
    api.get('/system/services').then((r) => setUnits(r.units || [])).catch(() => {});
    api.get('/system/audit').then((r) => setAudit(r.entries || [])).catch(() => {});
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => { load(); }, [load]);

  async function act(unit, action) {
    msg.clear();
    setBusy(unit + action);
    try {
      await api.post(`/system/services/${unit}/${action}`, {});
      msg.ok(`${unit} ${action}ed`);
    } catch (err) {
      msg.fail(err);
    }
    setBusy(null);
    await load();
  }

  return (
    <>
      <Message value={msg.message} onClear={msg.clear} />

      {info && (
        <Card title="This server">
          <dl className="kv">
            <dt>Panel</dt><dd>{info.panel_version}</dd>
            <dt>Operating system</dt>
            <dd>{info.distro ? `${info.distro.name || ''} ${info.distro.version || ''}`.trim() : '—'}</dd>
            <dt>Webserver</dt><dd>{info.webserver_backend}</dd>
            <dt>PHP provider</dt><dd>{info.php_provider}</dd>
          </dl>
        </Card>
      )}

      <Card title="Services" actions={<button type="button" onClick={load}>Refresh</button>}>
        {units.length === 0 ? (
          <Empty>No services reported.</Empty>
        ) : (
          <div className="scroll">
            <table>
              <thead>
                <tr><th>Unit</th><th>State</th><th>Enabled</th><th /></tr>
              </thead>
              <tbody>
                {units.map((u) => (
                  <tr key={u.unit}>
                    <td><code>{u.unit}</code></td>
                    <td>
                      {u.active === 'active'
                        ? <Tag kind="ok">running</Tag>
                        : <Tag kind="bad">{u.active || 'stopped'}</Tag>}
                    </td>
                    <td className="muted">{u.enabled || '—'}</td>
                    <td className="right nowrap">
                      {['start', 'stop', 'restart'].map((a) => (
                        <span key={a}>
                          <button
                            type="button"
                            className="link"
                            disabled={busy === u.unit + a}
                            onClick={() => act(u.unit, a)}
                          >
                            {a}
                          </button>{' '}
                        </span>
                      ))}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>

      <Card title="Recent activity">
        {audit.length === 0 ? (
          <Empty>Nothing recorded yet.</Empty>
        ) : (
          <div className="scroll">
            <table>
              <thead>
                <tr><th>Time</th><th>Who</th><th>Action</th><th>Target</th><th>Result</th></tr>
              </thead>
              <tbody>
                {audit.slice(0, 100).map((e) => (
                  <tr key={e.id}>
                    <td className="muted nowrap">{fmtDate(e.at)}</td>
                    <td>{e.actor_name || <span className="muted">—</span>}</td>
                    <td><code>{e.action}</code></td>
                    <td className="muted">{e.target}</td>
                    <td>
                      {e.ok ? <Tag kind="ok">ok</Tag> : <Tag kind="bad">failed</Tag>}
                      {e.detail && (
                        <div className="muted" style={{ fontSize: '.8em' }}>{e.detail}</div>
                      )}
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
