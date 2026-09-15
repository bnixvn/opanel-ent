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

  async function installManager() {
    msg.clear();
    setBusy(true);
    try {
      await api.post('/cloudlinux/manager', {});
      msg.ok('CloudLinux Manager is installed and running.');
    } catch (err) {
      msg.fail(err);
    }
    setBusy(false);
    load(period);
  }

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

      {state && st?.manager_running && (
        <Card title="CloudLinux Manager">
          <iframe
            title="CloudLinux Manager"
            src="/lvemanager/"
            style={{
              width: '100%', height: '78vh', border: 0,
              borderRadius: '.4rem', background: '#fff',
            }}
          />
        </Card>
      )}

      {state && st?.installed && !st?.manager_running && (
        <Card title="CloudLinux Manager">
          <p className="muted">
            CloudLinux's own interface: current usage, users, statistics,
            options, packages and the selectors. The panel serves it and tells
            it who is asking, so it does not ask again -- the alternative
            CloudLinux ships is a service on a port of its own that wants a
            system password.
          </p>
          <button type="button" disabled={busy} onClick={installManager}>
            {busy ? 'Installing…' : (st.manager ? 'Start it' : 'Install it')}
          </button>
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
