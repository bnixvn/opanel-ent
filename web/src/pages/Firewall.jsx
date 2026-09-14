import React, { useCallback, useEffect, useState } from 'react';
import { api, fmtDate } from '../api.js';
import { Card, Empty, Message, Tag, useConfirm, useMessage } from '../components.jsx';

export default function Firewall() {
  const [data, setData] = useState(null);
  const [busy, setBusy] = useState(false);
  const msg = useMessage();
  const { ask, dialog } = useConfirm();

  const load = useCallback(async () => {
    try {
      setData(await api.get('/firewall'));
    } catch (err) {
      msg.fail(err);
    }
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => { load(); }, [load]);

  // Every change is provisional until this call lands. Reaching it is the
  // proof the new rules did not cut off the connection that made them; if
  // they did, nothing arrives and the server puts the old rules back by
  // itself.
  async function change(fn, okText) {
    setBusy(true);
    msg.clear();
    try {
      await fn();
      await api.post('/firewall/confirm', {});
      msg.ok(okText);
    } catch (err) {
      msg.fail(err);
    }
    setBusy(false);
    await load();
  }

  if (!data) return <Message value={msg.message} onClear={msg.clear} />;

  const status = data.status || {};
  const ports = data.rules.filter((r) => r.kind === 'port');
  const blocks = data.rules.filter((r) => r.kind === 'block');
  const allows = data.rules.filter((r) => r.kind === 'allow');

  return (
    <>
      {dialog}
      <Message value={msg.message} onClear={msg.clear} />

      <Card title="Status">
        <dl className="kv">
          <dt>Firewall</dt>
          <dd>
            {status.active ? <Tag kind="ok">running</Tag> : <Tag kind="bad">not loaded</Tag>}
            {status.pending_confirm && <> <Tag kind="warn">waiting to be confirmed</Tag></>}
            {status.error && <div className="muted" style={{ fontSize: '.83rem' }}>{status.error}</div>}
          </dd>
          <dt>Open ports</dt><dd>{status.loaded_rules ?? ports.length}</dd>
          <dt>Blocked addresses</dt><dd>{status.blocklist_size ?? 0} from subscriptions</dd>
        </dl>
        <p className="muted" style={{ fontSize: '.83rem', marginBottom: 0 }}>
          SSH and the panel&apos;s own port are always open and cannot be closed
          from here. Every change reverts by itself after two minutes unless
          this page confirms it worked — which is what stops a firewall change
          locking you out for good.
        </p>
      </Card>

      <RuleForm kind="port" busy={busy} onSubmit={change} />

      <Card title="Open ports">
        <div className="scroll">
          <table>
            <thead>
              <tr><th>Port</th><th>Protocol</th><th>From</th><th>Note</th><th /></tr>
            </thead>
            <tbody>
              {(status.protected_ports || []).map((p) => (
                <tr key={`protected-${p.port}`}>
                  <td><code>{p.port}</code></td>
                  <td className="muted">{p.protocol}</td>
                  <td className="muted">anywhere</td>
                  <td className="muted">{p.reason}</td>
                  <td className="right">
                    <Tag kind="mute">always open</Tag>
                  </td>
                </tr>
              ))}
              {ports.map((r) => (
                <tr key={r.id}>
                  <td>
                    <code>{r.port_to > r.port_from ? `${r.port_from}-${r.port_to}` : r.port_from}</code>
                  </td>
                  <td className="muted">{r.protocol}</td>
                  <td className="muted">{r.address || 'anywhere'}</td>
                  <td className="muted">{r.comment}</td>
                  <td className="right">
                    <button
                      type="button"
                      className="link"
                      disabled={busy}
                      onClick={async () => {
                        const ok = await ask({
                          title: `Close port ${r.port_from}?`,
                          body: 'Whatever is listening on it stops being reachable from '
                            + 'outside. The change reverts by itself if this page cannot '
                            + 'confirm it afterwards.',
                          confirmLabel: 'Close it',
                          danger: true,
                        });
                        if (ok) change(() => api.del(`/firewall/rules/${r.id}`), 'Port closed');
                      }}
                    >
                      Close
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <p className="muted" style={{ fontSize: '.83rem', marginBottom: 0 }}>
          SSH and the panel have no Close button on purpose: closing them from
          here is how an operator locks themselves out of their own server.
        </p>
      </Card>

      <RuleForm kind="block" busy={busy} onSubmit={change} />

      <Card title="Blocked addresses">
        {blocks.length === 0 ? <Empty>Nothing blocked by hand.</Empty> : (
          <RuleTable rows={blocks} busy={busy} ask={ask} change={change} />
        )}
      </Card>

      <RuleForm kind="allow" busy={busy} onSubmit={change} />

      <Card title="Always allowed">
        {allows.length === 0 ? <Empty>No exceptions.</Empty> : (
          <RuleTable rows={allows} busy={busy} ask={ask} change={change} />
        )}
        <p className="muted" style={{ fontSize: '.83rem', marginBottom: 0 }}>
          Checked before the blocklists, so your own office address is not caught
          by a feed you subscribed to.
        </p>
      </Card>

      <Sources sources={data.sources} busy={busy} msg={msg} reload={load} ask={ask} />
    </>
  );
}

function RuleTable({ rows, busy, ask, change, portish }) {
  return (
    <div className="scroll">
      <table>
        <thead>
          <tr>
            {portish ? <><th>Port</th><th>Protocol</th><th>From</th></> : <th>Address</th>}
            <th>Note</th><th />
          </tr>
        </thead>
        <tbody>
          {rows.map((r) => (
            <tr key={r.id}>
              {portish ? (
                <>
                  <td><code>{r.port_to > r.port_from ? `${r.port_from}-${r.port_to}` : r.port_from}</code></td>
                  <td className="muted">{r.protocol}</td>
                  <td className="muted">{r.address || 'anywhere'}</td>
                </>
              ) : (
                <td><code>{r.address}</code></td>
              )}
              <td className="muted">{r.comment}</td>
              <td className="right">
                <button
                  type="button"
                  className="link"
                  disabled={busy}
                  onClick={async () => {
                    const ok = await ask({
                      title: 'Remove this rule?',
                      body: 'The firewall is reloaded straight away, and reverts by '
                        + 'itself if this page cannot confirm it afterwards.',
                      confirmLabel: 'Remove',
                      danger: true,
                    });
                    if (ok) change(() => api.del(`/firewall/rules/${r.id}`), 'Rule removed');
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
  );
}

const TITLES = {
  port: 'Open a port',
  block: 'Block an address',
  allow: 'Always allow an address',
};

function RuleForm({ kind, busy, onSubmit }) {
  const [form, setForm] = useState({
    protocol: 'tcp', port_from: '', port_to: '', address: '', comment: '',
  });
  const set = (k) => (e) => setForm({ ...form, [k]: e.target.value });

  return (
    <Card title={TITLES[kind]}>
      <form
        className="row"
        onSubmit={(e) => {
          e.preventDefault();
          const body = {
            kind,
            protocol: form.protocol,
            port_from: Number(form.port_from) || 0,
            port_to: Number(form.port_to) || 0,
            address: form.address.trim(),
            comment: form.comment.trim(),
          };
          onSubmit(() => api.post('/firewall/rules', body), 'Rule added');
          setForm({ ...form, port_from: '', port_to: '', address: '', comment: '' });
        }}
      >
        {kind === 'port' && (
          <>
            <div className="field" style={{ flex: '0 0 7rem' }}>
              <label htmlFor={`${kind}From`}>Port</label>
              <input
                id={`${kind}From`}
                type="number"
                min="1"
                max="65535"
                required
                value={form.port_from}
                onChange={set('port_from')}
              />
            </div>
            <div className="field" style={{ flex: '0 0 7rem' }}>
              <label htmlFor={`${kind}To`}>to (optional)</label>
              <input id={`${kind}To`} type="number" min="1" max="65535" value={form.port_to} onChange={set('port_to')} />
            </div>
            <div className="field" style={{ flex: '0 0 7rem' }}>
              <label htmlFor={`${kind}Proto`}>Protocol</label>
              <select id={`${kind}Proto`} value={form.protocol} onChange={set('protocol')}>
                <option value="tcp">tcp</option>
                <option value="udp">udp</option>
                <option value="both">both</option>
              </select>
            </div>
          </>
        )}
        <div className="field">
          <label htmlFor={`${kind}Addr`}>
            {kind === 'port' ? 'Only from (optional)' : 'Address or range'}
          </label>
          <input
            id={`${kind}Addr`}
            placeholder="203.0.113.4 or 203.0.113.0/24"
            required={kind !== 'port'}
            value={form.address}
            onChange={set('address')}
          />
        </div>
        <div className="field">
          <label htmlFor={`${kind}Note`}>Note</label>
          <input id={`${kind}Note`} value={form.comment} onChange={set('comment')} />
        </div>
        <div><button type="submit" className="primary" disabled={busy}>Add</button></div>
      </form>
    </Card>
  );
}

function Sources({ sources, busy, msg, reload, ask }) {
  const [url, setUrl] = useState('');
  const [description, setDescription] = useState('');

  async function run(fn, okText) {
    msg.clear();
    try {
      const res = await fn();
      msg.ok(res && res.warning ? res.warning : okText);
    } catch (err) {
      msg.fail(err);
    }
    await reload();
  }

  return (
    <Card title="Blocklist subscriptions">
      <form
        className="row"
        onSubmit={(e) => {
          e.preventDefault();
          run(
            () => api.post('/firewall/sources', { url: url.trim(), description: description.trim() }),
            'Blocklist added and fetched',
          );
          setUrl('');
          setDescription('');
        }}
      >
        <div className="field">
          <label htmlFor="blUrl">Address of a .txt list</label>
          <input
            id="blUrl"
            type="url"
            required
            placeholder="https://example.com/blocklist.txt"
            value={url}
            onChange={(e) => setUrl(e.target.value)}
          />
        </div>
        <div className="field">
          <label htmlFor="blDesc">Note</label>
          <input id="blDesc" value={description} onChange={(e) => setDescription(e.target.value)} />
        </div>
        <div><button type="submit" className="primary" disabled={busy}>Add</button></div>
      </form>

      {sources.length === 0 ? (
        <Empty>No subscriptions.</Empty>
      ) : (
        <div className="scroll">
          <table>
            <thead>
              <tr><th>List</th><th>Addresses</th><th>Last fetched</th><th /></tr>
            </thead>
            <tbody>
              {sources.map((s) => (
                <tr key={s.id}>
                  <td>
                    <code style={{ wordBreak: 'break-all' }}>{s.url}</code>
                    {s.description && <div className="muted" style={{ fontSize: '.85em' }}>{s.description}</div>}
                    {s.last_error && <div className="tag bad" style={{ marginTop: '.2rem' }}>{s.last_error}</div>}
                  </td>
                  <td className="muted">{s.entry_count}</td>
                  <td className="muted">{s.last_fetch_at ? fmtDate(s.last_fetch_at) : 'never'}</td>
                  <td className="right nowrap">
                    <button
                      type="button"
                      className="link"
                      disabled={busy}
                      onClick={() => run(() => api.post(`/firewall/sources/${s.id}/refresh`, {}), 'Refreshed')}
                    >
                      Refresh
                    </button>{' '}
                    <button
                      type="button"
                      className="link"
                      disabled={busy}
                      onClick={async () => {
                        const ok = await ask({
                          title: 'Remove this blocklist?',
                          body: 'Every address it contributed stops being blocked.',
                          confirmLabel: 'Remove',
                          danger: true,
                        });
                        if (ok) run(() => api.del(`/firewall/sources/${s.id}`), 'Blocklist removed');
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
      <p className="muted" style={{ fontSize: '.83rem', marginBottom: 0 }}>
        One address or range per line; <code>#</code> comments are ignored.
        Refetched on each list&apos;s own schedule. A list that has been failing
        is shown here rather than quietly leaving you unprotected.
      </p>
    </Card>
  );
}
