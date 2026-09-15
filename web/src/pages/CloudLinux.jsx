import React, { useCallback, useEffect, useState } from 'react';
import { api } from '../api.js';
import { Card, Empty, Message, Tag, useMessage } from '../components.jsx';

export default function CloudLinux() {
  const [state, setState] = useState(null);
  const [busy, setBusy] = useState(false);
  const msg = useMessage();

  const load = useCallback(() => {
    api.get('/cloudlinux').then(setState).catch(msg.fail);
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => { load(); }, [load]);

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
    load();
  }

  async function setupSelector() {
    msg.clear();
    setBusy(true);
    try {
      await api.post('/cloudlinux/selector', {});
      msg.ok('PHP Selector can now offer every alt-php version installed here.');
    } catch (err) {
      msg.fail(err);
    }
    setBusy(false);
    load();
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
    load();
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

      {state && st?.installed && (
        <Card title="PHP Selector">
          <p className="muted">
            The versions a customer can choose for themselves, from
            CloudLinux's own alt-php builds. Separate from the version a site
            runs, which the panel sets per site: this is the interpreter inside
            the customer's own shell and cron. Setting it up installs every
            alt-php CloudLinux ships (about 2&nbsp;GB) and rebuilds the CageFS
            skeleton around them, which takes a few minutes.
          </p>
          <dl className="kv">
            <dt>Offers</dt>
            <dd>
              {st.selector?.versions?.length
                ? st.selector.versions.join(', ')
                : <Tag kind="bad">nothing yet</Tag>}
            </dd>
            <dt>Native</dt>
            <dd>
              {st.selector?.native
                ? <code>{st.selector.native}</code>
                : <Tag kind="bad">not declared</Tag>}
            </dd>
          </dl>
          <button type="button" disabled={busy || !st.selector?.available} onClick={setupSelector}>
            {busy ? 'Working…' : (st.selector?.versions?.length ? 'Install the rest' : 'Set it up')}
          </button>
          {!st.selector?.available && (
            <p className="muted">
              CageFS has to be installed first: the selector works by giving
              each customer a different interpreter inside their own cage.
            </p>
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
