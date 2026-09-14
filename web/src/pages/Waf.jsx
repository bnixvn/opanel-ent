import React, { useCallback, useEffect, useState } from 'react';
import { api } from '../api.js';
import { Card, Empty, Hint, Message, Tag, useConfirm, useMessage } from '../components.jsx';

const MODES = [
  { id: 'detect', label: 'Log only' },
  { id: 'block', label: 'Block' },
  { id: 'off', label: 'Off' },
];

export default function Waf() {
  const [data, setData] = useState(null);
  const [events, setEvents] = useState([]);
  const [mode, setMode] = useState('detect');
  const [excluded, setExcluded] = useState('');
  // The rule set as categories, and which ones are switched off.
  const [rules, setRules] = useState([]);
  const [disabled, setDisabled] = useState([]);
  const [busy, setBusy] = useState(false);
  const msg = useMessage();
  const { ask, dialog } = useConfirm();

  const load = useCallback(async () => {
    try {
      setData(await api.get('/waf'));
    } catch (err) {
      msg.fail(err);
    }
    try {
      setEvents((await api.get('/waf/events')).events || []);
    } catch { /* no audit log yet is normal */ }
    try {
      const res = await api.get('/waf/rules');
      setRules(res.rules || []);
      setDisabled((res.rules || []).filter((x) => !x.enabled).map((x) => x.file));
    } catch { /* the rule set may not be installed */ }
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => { load(); }, [load]);

  async function run(fn, okText) {
    setBusy(true);
    msg.clear();
    try {
      await fn();
      msg.ok(okText);
    } catch (err) {
      msg.fail(err);
    }
    setBusy(false);
    await load();
  }

  if (!data) return <Message value={msg.message} onClear={msg.clear} />;
  const waf = data.waf || {};

  return (
    <>
      {dialog}
      <Message value={msg.message} onClear={msg.clear} />

      <Card title="Status">
        <dl className="kv">
          <dt>Engine</dt>
          <dd>
            {waf.module_available
              ? <Tag kind="ok">available</Tag>
              : <Tag kind="bad">not present in this webserver</Tag>}
          </dd>
          <dt>Rules</dt>
          <dd>
            {waf.rules_installed
              ? <>
                <Tag kind="ok">{waf.rule_count} rules</Tag>
                {waf.rules_version && <> <Tag kind="mute">CRS {waf.rules_version}</Tag></>}
              </>
              : <Tag kind="warn">not installed</Tag>}
          </dd>
        </dl>

        {waf.module_available && !waf.rules_installed && (
          <p style={{ marginBottom: 0 }}>
            <button
              type="button"
              className="primary"
              disabled={busy}
              onClick={() => run(() => api.post('/waf/install', {}), 'Rule set installed in log-only mode')}
            >
              {busy ? 'Installing…' : 'Install the OWASP rule set'}
            </button>
            <span className="muted"> Downloads the Core Rule Set and starts in log-only mode.</span>
          </p>
        )}
        {!waf.module_available && (
          <p className="muted" style={{ marginBottom: 0, fontSize: '.85rem' }}>
            This webserver build has no ModSecurity module, so there is nothing
            to configure here.
          </p>
        )}
      </Card>

      {waf.rules_installed && (
        <Card title="Mode">
          <form
            className="row"
            onSubmit={(e) => {
              e.preventDefault();
              const ids = excluded.split(/[\s,]+/).filter(Boolean);
              run(
                () => api.put('/waf', { mode, excluded_rules: ids, disabled_files: disabled }),
                `WAF set to ${MODES.find((m) => m.id === mode).label.toLowerCase()}`,
              );
            }}
          >
            <div className="field" style={{ flex: '0 0 12rem' }}>
              <label htmlFor="wafMode">Mode</label>
              <select id="wafMode" value={mode} onChange={(e) => setMode(e.target.value)}>
                {MODES.map((m) => <option key={m.id} value={m.id}>{m.label}</option>)}
              </select>
            </div>
            <div className="field">
              <label htmlFor="wafEx">Rule ids to switch off</label>
              <input
                id="wafEx"
                placeholder="941100 942100"
                value={excluded}
                onChange={(e) => setExcluded(e.target.value)}
              />
            </div>
            <div><button type="submit" className="primary" disabled={busy}>Save</button></div>
          </form>
        </Card>
      )}

      {waf.rules_installed && rules.length > 0 && (
        <Card title="Rule set">
          <div className="scroll">
            <table>
              <thead>
                <tr><th style={{ width: '1.6rem' }} /><th>Protects against</th><th>Rules</th><th>When</th></tr>
              </thead>
              <tbody>
                {rules.map((r) => (
                  <tr key={r.file} className={disabled.includes(r.file) ? undefined : 'selected'}>
                    <td>
                      <input
                        type="checkbox"
                        aria-label={r.title}
                        disabled={r.required}
                        checked={r.required || !disabled.includes(r.file)}
                        onChange={() => setDisabled(
                          disabled.includes(r.file)
                            ? disabled.filter((f) => f !== r.file)
                            : [...disabled, r.file],
                        )}
                      />
                    </td>
                    <td>
                      {r.title}
                      {r.required && <> <Tag kind="mute">needed</Tag></>}
                      <Hint>{r.help}</Hint>
                      <div className="muted" style={{ fontSize: '.72rem' }}><code>{r.file}</code></div>
                    </td>
                    <td className="muted nowrap">{r.rules || '—'}</td>
                    <td className="muted nowrap">{r.phase}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Card>
      )}

      <Card title="Websites">
        {data.sites.length === 0 ? <Empty>No websites yet.</Empty> : (
          <div className="scroll">
            <table>
              <thead><tr><th>Domain</th><th>WAF</th><th /></tr></thead>
              <tbody>
                {data.sites.map((s) => (
                  <tr key={s.id}>
                    <td><code>{s.domain}</code></td>
                    <td>{s.enabled ? <Tag kind="ok">on</Tag> : <Tag>off</Tag>}</td>
                    <td className="right">
                      <button
                        type="button"
                        disabled={busy || !waf.rules_installed}
                        onClick={() => run(
                          () => api.post(`/sites/${s.id}/waf`, { enabled: !s.enabled }),
                          `WAF ${s.enabled ? 'off' : 'on'} for ${s.domain}`,
                        )}
                      >
                        {s.enabled ? 'Turn off' : 'Turn on'}
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>

      <Card title="Recent matches" actions={<button type="button" onClick={load}>Refresh</button>}>
        {events.length === 0 ? (
          <Empty>Nothing recorded yet.</Empty>
        ) : (
          <div className="scroll">
            <table>
              <thead>
                <tr><th>When</th><th>From</th><th>Request</th><th>Rule</th><th /></tr>
              </thead>
              <tbody>
                {events.map((e, i) => (
                  <tr key={`${e.at}-${e.rule_id}-${i}`}>
                    <td className="muted nowrap">{e.at}</td>
                    <td className="muted">{e.client_ip}</td>
                    <td>
                      <code style={{ wordBreak: 'break-all' }}>{e.host}{e.uri}</code>
                    </td>
                    <td>
                      <code>{e.rule_id}</code>
                      {e.message && <div className="muted" style={{ fontSize: '.8em' }}>{e.message}</div>}
                    </td>
                    <td>{e.blocked ? <Tag kind="bad">blocked</Tag> : <Tag kind="warn">logged</Tag>}</td>
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
