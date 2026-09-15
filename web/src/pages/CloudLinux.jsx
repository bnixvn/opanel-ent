import React, { useCallback, useEffect, useState } from 'react';
import { api } from '../api.js';
import { Card, Empty, Message, Tag, useMessage } from '../components.jsx';

const PERIODS = [
  ['5m', 'last 5 minutes'],
  ['4h', 'last 4 hours'],
  ['1d', 'last 24 hours'],
  ['7d', 'last 7 days'],
];

export default function CloudLinux() {
  const [state, setState] = useState(null);
  const [period, setPeriod] = useState('1d');
  const [busy, setBusy] = useState(false);
  const msg = useMessage();

  const load = useCallback((p) => {
    api.get(`/cloudlinux?period=${encodeURIComponent(p)}`).then(setState).catch(msg.fail);
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => { load(period); }, [load, period]);

  async function installIntegration() {
    msg.clear();
    setBusy(true);
    try {
      await api.post('/cloudlinux/integration', {});
      msg.ok('CloudLinux can now read this panel.');
    } catch (err) {
      msg.fail(err);
    }
    setBusy(false);
    load(period);
  }

  const st = state?.status;

  if (state && !st?.installed) {
    return (
      <>
        <Message value={msg.message} onClear={msg.clear} />
        <Card title="CloudLinux">
          <Empty>
            CloudLinux is not installed on this server. It is a conversion of
            the running AlmaLinux rather than a package, and it reboots, so the
            panel does not offer to do it for you.
          </Empty>
        </Card>
      </>
    );
  }

  return (
    <>
      <Message value={msg.message} onClear={msg.clear} />

      <Card title="CloudLinux">
        {!state ? <Empty>Loading.</Empty> : (
          <dl className="kv">
            <dt>System</dt><dd>{st.os || '—'}</dd>
            <dt>Edition</dt><dd>{st.edition || '—'}</dd>
            <dt>Licence</dt>
            <dd>
              {st.licence === 'OK'
                ? <Tag kind="ok">valid</Tag>
                : <Tag kind="bad">{st.licence || 'unknown'}</Tag>}
            </dd>
            <dt>Limits enforced</dt>
            <dd>
              {st.lve
                ? <Tag kind="ok">the LVE kernel module is loaded</Tag>
                : <Tag kind="bad">the LVE module is not loaded — limits are recorded but not applied</Tag>}
            </dd>
            <dt>Reads this panel</dt>
            <dd>
              {st.integration
                ? <Tag kind="ok">yes</Tag>
                : (
                  <>
                    <Tag kind="bad">no</Tag>{' '}
                    <button type="button" disabled={busy} onClick={installIntegration}>
                      {busy ? 'Installing…' : 'Let it'}
                    </button>
                  </>
                )}
            </dd>
          </dl>
        )}
      </Card>

      {state && (
        <Card title="Resource limits">
          <p className="muted">
            What one account may use before the kernel makes it wait. A zero
            means no limit. These are CloudLinux's own defaults until the panel
            sets them per package.
          </p>
          {(state.limits || []).length === 0 ? (
            <Empty>No limits reported. Is lvectl installed?</Empty>
          ) : (
            <div className="scroll">
              <table>
                <thead>
                  <tr>
                    <th>Applies to</th><th>Speed</th><th>Memory</th>
                    <th>Entry processes</th><th>Processes</th><th>IO</th><th>IOPS</th>
                  </tr>
                </thead>
                <tbody>
                  {state.limits.map((l) => (
                    <tr key={l.id}>
                      <td><code>{l.id}</code></td>
                      <td>{l.speed}%</td>
                      <td>{l.pmem}</td>
                      <td>{l.ep}</td>
                      <td>{l.nproc}</td>
                      <td>{l.io}</td>
                      <td>{l.iops}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Card>
      )}

      {state && (
        <Card
          title="What accounts are actually using"
          actions={(
            <select value={period} onChange={(e) => setPeriod(e.target.value)}>
              {PERIODS.map(([v, label]) => <option key={v} value={v}>{label}</option>)}
            </select>
          )}
        >
          <p className="muted">
            A fault is a request that hit a limit: somebody waiting, or a page
            that did not load. An account with faults either needs a bigger
            package or has code worth looking at — it is the column to read
            first.
          </p>
          {(state.usage || []).length === 0 ? (
            <Empty>Nothing recorded for this period.</Empty>
          ) : (
            <div className="scroll">
              <table>
                <thead>
                  <tr>
                    <th>Account</th><th>CPU avg</th><th>CPU peak</th>
                    <th>Memory peak</th><th>Processes</th><th>Faults</th>
                  </tr>
                </thead>
                <tbody>
                  {state.usage.map((u) => {
                    const faults = (u.faults_ep || 0) + (u.faults_mem || 0) + (u.faults_proc || 0);
                    return (
                      <tr key={u.uid}>
                        <td>{u.username || <code>uid {u.uid}</code>}</td>
                        <td>{pct(u.cpu_avg)}</td>
                        <td>{pct(u.cpu_max)}{u.cpu_limit ? <span className="muted"> / {pct(u.cpu_limit)}</span> : null}</td>
                        <td>{mb(u.mem_max_bytes)}{u.mem_limit_bytes ? <span className="muted"> / {mb(u.mem_limit_bytes)}</span> : null}</td>
                        <td>{u.proc_max || 0}</td>
                        <td>{faults > 0 ? <Tag kind="bad">{faults}</Tag> : <span className="muted">none</span>}</td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          )}
        </Card>
      )}

      {state && st && (
        <Card title="Components">
          <p className="muted">
            What CloudLinux has installed here. The panel reports rather than
            installs these: each one changes how the server runs, and CageFS in
            particular changes what a customer's shell can see.
          </p>
          <dl className="kv">
            {Object.entries(st.tools || {}).map(([name, present]) => (
              <React.Fragment key={name}>
                <dt><code>{name}</code></dt>
                <dd>{present ? <Tag kind="ok">installed</Tag> : <Tag>not installed</Tag>}</dd>
              </React.Fragment>
            ))}
          </dl>
        </Card>
      )}
    </>
  );
}

function pct(v) {
  if (!v && v !== 0) return '—';
  return `${Math.round(v)}%`;
}

function mb(bytes) {
  if (!bytes) return '—';
  const m = bytes / 1024 / 1024;
  return m >= 1024 ? `${(m / 1024).toFixed(1)} GB` : `${Math.round(m)} MB`;
}
