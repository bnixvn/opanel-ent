import React, { useCallback, useEffect, useState } from 'react';
import { api } from '../api.js';
import { Card, Empty, Message, Tag, useConfirm, useMessage } from '../components.jsx';

// PasskeySwitch is the administrator's activation button.
//
// It stays disabled, with the reasons spelled out, until the server can
// actually support passkeys: a domain name for the panel and a certificate a
// browser trusts. Switching it on anyway would put a button on the login
// page that fails inside the browser, which reads as the panel being broken
// rather than the host being unconfigured.
function PasskeySwitch({ msg }) {
  const [state, setState] = useState(null);

  const load = useCallback(async () => {
    try {
      const res = await api.get('/auth/passkeys');
      setState(res.state);
    } catch { /* not fatal: the rest of the page still works */ }
  }, []);

  useEffect(() => { load(); }, [load]);

  if (!state) return null;

  return (
    <Card title="Passkeys">
      <p className="muted" style={{ marginTop: 0, fontSize: '.85rem' }}>
        A passkey replaces the two-factor code: after the password, the
        account holder confirms with their phone, laptop or security key. It
        cannot be phished or reused from somebody else&apos;s breach, because
        the signature is bound to this server&apos;s own address.
      </p>

      {state.ready ? (
        <>
          <div className="row" style={{ alignItems: 'center' }}>
            <div>
              <button
                type="button"
                className={state.enabled ? 'danger' : 'primary'}
                onClick={async () => {
                  try {
                    const res = await api.put('/settings/passkeys', { enabled: !state.enabled });
                    setState(res.state);
                    msg.ok(res.state.enabled
                      ? 'Passkeys are on. Everyone can add one from their Account page.'
                      : 'Passkeys are off. Registered keys are kept but will not be asked for.');
                  } catch (err) {
                    msg.fail(err);
                  }
                }}
              >
                {state.enabled ? 'Turn passkeys off' : 'Turn passkeys on'}
              </button>
            </div>
            <p className="muted" style={{ flex: 1, margin: 0, fontSize: '.83rem' }}>
              Passkeys will be bound to <code>{state.hostname}</code> and will
              only work at <code>{state.origin}</code>. Changing the
              panel&apos;s hostname later makes every registered key unusable,
              so pick the address people will keep using.
            </p>
          </div>
          <p style={{ marginBottom: 0 }}>
            <Tag kind={state.enabled ? 'ok' : 'mute'}>
              {state.enabled ? 'on' : 'off'}
            </Tag>
          </p>
        </>
      ) : (
        <>
          <p style={{ margin: '0 0 .4rem' }}>
            <Tag kind="warn">not available yet</Tag>
          </p>
          <ul className="muted" style={{ fontSize: '.85rem', margin: '0 0 .6rem', paddingLeft: '1.1rem' }}>
            {(state.blockers || []).map((b) => <li key={b}>{b}</li>)}
          </ul>
          <button type="button" disabled>Turn passkeys on</button>
        </>
      )}
    </Card>
  );
}

export default function Settings() {
  const [data, setData] = useState(null);
  const msg = useMessage();
  const { ask, dialog } = useConfirm();

  const load = useCallback(async () => {
    try {
      setData(await api.get('/settings'));
    } catch (err) {
      msg.fail(err);
    }
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => { load(); }, [load]);

  if (!data) return <Message value={msg.message} onClear={msg.clear} />;

  const net = data.network || {};

  return (
    <>
      {dialog}
      <Message value={msg.message} onClear={msg.clear} />

      <Branding branding={data.branding} msg={msg} onSaved={load} />

      <Card title="This server">
        <dl className="kv">
          <dt>Hostname</dt><dd>{net.hostname || '—'}</dd>
          <dt>IPv4</dt><dd><code>{net.primary_ipv4 || 'none'}</code></dd>
          <dt>IPv6</dt>
          <dd>
            {net.primary_ipv6 ? <code>{net.primary_ipv6}</code> : <span className="muted">none detected</span>}
          </dd>
          <dt>Panel listens on</dt><dd><code>{data.listen}</code>{data.tls ? ' (HTTPS)' : ' (HTTP)'}</dd>
          <dt>Disk quota</dt>
          <dd>
            {data.quota && data.quota.enforced
              ? <Tag kind="ok">enforced by the filesystem</Tag>
              : (
                <>
                  <Tag kind="warn">
                    {data.quota && data.quota.pending_reboot ? 'waiting for a reboot' : 'not enforced'}
                  </Tag>
                  <div className="muted" style={{ fontSize: '.83rem' }}>
                    {data.quota && data.quota.pending_reboot
                      ? 'The kernel argument is set. Reboot the server and package '
                        + 'disk limits start being enforced by the filesystem.'
                      : 'Package disk limits are advisory until project quota is enabled.'}
                    {data.quota && data.quota.mount_options && (
                      <div><code>{data.quota.mount_options}</code></div>
                    )}
                  </div>
                </>
              )}
          </dd>
        </dl>

        <div className="row" style={{ marginTop: '.8rem' }}>
          <div className="field" style={{ flex: '0 0 12rem' }}>
            <label htmlFor="v6">IPv6</label>
            <select
              id="v6"
              value={data.ipv6_enabled ? '1' : '0'}
              disabled={!net.ipv6_available}
              onChange={async (e) => {
                try {
                  await api.put('/settings/ipv6', { enabled: e.target.value === '1' });
                  msg.ok('Saved. Websites are re-rendered on the next configuration sync.');
                } catch (err) {
                  msg.fail(err);
                }
                load();
              }}
            >
              <option value="0">off</option>
              <option value="1">on</option>
            </select>
          </div>
          {!net.ipv6_available && (
            <p className="muted" style={{ fontSize: '.83rem', alignSelf: 'center' }}>
              This server has no routable IPv6 address, so there is nothing to
              turn on.
            </p>
          )}
        </div>
      </Card>

      <PasskeySwitch msg={msg} />

      <Card title="Panel addresses">
        <p className="muted" style={{ marginTop: 0, fontSize: '.85rem' }}>
          The panel answers on these names only. Without this list, anybody who
          points a domain at this server could put a login page for your panel
          on their own address.
        </p>
        {(data.hostnames || []).length === 0 ? (
          <Empty>Only the address you are using now.</Empty>
        ) : (
          <div className="scroll">
            <table>
              <thead><tr><th>Hostname</th><th>Certificate</th><th /></tr></thead>
              <tbody>
                {data.hostnames.map((h) => (
                  <tr key={h.hostname}>
                    <td>
                      <code>{h.hostname}</code>
                      {h.is_primary && <> <Tag kind="ok">primary</Tag></>}
                    </td>
                    <td>
                      {h.has_certificate ? <Tag kind="ok">installed</Tag> : <Tag kind="warn">none</Tag>}
                    </td>
                    <td className="right">
                      <button
                        type="button"
                        className="link"
                        onClick={async () => {
                          const ok = await ask({
                            title: `Stop serving the panel on ${h.hostname}?`,
                            body: 'Anyone using that address will no longer reach the panel.',
                            confirmLabel: 'Remove',
                            danger: true,
                          });
                          if (!ok) return;
                          try {
                            await api.del(`/settings/hostnames?hostname=${encodeURIComponent(h.hostname)}`);
                            msg.ok('Removed');
                          } catch (err) {
                            msg.fail(err);
                          }
                          load();
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
        <AddHostname msg={msg} onSaved={load} />
      </Card>
    </>
  );
}

function AddHostname({ msg, onSaved }) {
  const [hostname, setHostname] = useState('');
  const [primary, setPrimary] = useState(false);
  return (
    <form
      className="row"
      style={{ marginTop: '.8rem' }}
      onSubmit={async (e) => {
        e.preventDefault();
        try {
          await api.post('/settings/hostnames', { hostname: hostname.trim(), is_primary: primary });
          msg.ok(`${hostname.trim()} added`);
          setHostname('');
        } catch (err) {
          msg.fail(err);
        }
        onSaved();
      }}
    >
      <div className="field">
        <label htmlFor="hn">Add a hostname</label>
        <input
          id="hn"
          placeholder="panel.example.com"
          required
          value={hostname}
          onChange={(e) => setHostname(e.target.value)}
        />
      </div>
      <div className="field" style={{ flex: '0 0 10rem' }}>
        <label htmlFor="hnPrimary">Primary</label>
        <select id="hnPrimary" value={primary ? '1' : '0'} onChange={(e) => setPrimary(e.target.value === '1')}>
          <option value="0">no</option>
          <option value="1">yes</option>
        </select>
      </div>
      <div><button type="submit" className="primary">Add</button></div>
    </form>
  );
}

function Branding({ branding, msg, onSaved }) {
  const [name, setName] = useState(branding.name || '');
  const [logo, setLogo] = useState('');
  const [favicon, setFavicon] = useState('');

  const readFile = (file, set) => {
    if (!file) return;
    const reader = new FileReader();
    reader.onload = () => set(String(reader.result));
    reader.readAsDataURL(file);
  };

  return (
    <Card title="Branding">
      <form
        className="row"
        onSubmit={async (e) => {
          e.preventDefault();
          try {
            await api.put('/settings/branding', { name, logo, favicon });
            msg.ok('Branding saved. Reload to see it everywhere.');
            setLogo('');
            setFavicon('');
          } catch (err) {
            msg.fail(err);
          }
          onSaved();
        }}
      >
        <div className="field">
          <label htmlFor="brName">Name</label>
          <input id="brName" value={name} onChange={(e) => setName(e.target.value)} />
        </div>
        <div className="field">
          <label htmlFor="brLogo">Logo</label>
          <input
            id="brLogo"
            type="file"
            accept="image/png,image/jpeg,image/webp,image/gif"
            onChange={(e) => readFile(e.target.files[0], setLogo)}
          />
        </div>
        <div className="field">
          <label htmlFor="brIcon">Favicon</label>
          <input
            id="brIcon"
            type="file"
            accept="image/png,image/jpeg,image/webp,image/gif"
            onChange={(e) => readFile(e.target.files[0], setFavicon)}
          />
        </div>
        <div><button type="submit" className="primary">Save</button></div>
      </form>
      <div style={{ marginTop: '.7rem', display: 'flex', gap: '1rem', alignItems: 'center' }}>
        {(logo || branding.logo) && (
          <img src={logo || branding.logo} alt="logo" style={{ maxHeight: '2.4rem' }} />
        )}
        {(favicon || branding.favicon) && (
          <img src={favicon || branding.favicon} alt="favicon" style={{ height: '1.2rem' }} />
        )}
        <span className="muted" style={{ fontSize: '.83rem' }}>
          PNG, JPEG, WebP or GIF, under 256 KB. SVG is refused because it can
          carry script and would run inside the panel.
        </span>
      </div>
    </Card>
  );
}
