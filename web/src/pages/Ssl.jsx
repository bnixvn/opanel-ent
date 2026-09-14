import React, { useCallback, useEffect, useState } from 'react';
import { api, fmtDate } from '../api.js';
import { Card, Empty, Message, Tag, useConfirm, useMessage } from '../components.jsx';

// Four ways to put a certificate on a site, because a hosting customer
// arrives with one of four situations: nothing yet, a wildcard they want for
// everything, a wildcard the server already holds, or a certificate they
// bought.
const METHODS = [
  { id: 'le', label: "Let's Encrypt", hint: 'Free, automatic, renewed by the panel. The domain must already point here.' },
  { id: 'wildcard', label: 'Wildcard (Cloudflare)', hint: 'Covers every subdomain. Needs a Cloudflare API token for the zone.' },
  { id: 'reuse', label: 'Reuse a certificate', hint: 'Use one this server already holds that covers this name.' },
  { id: 'manual', label: 'Upload a certificate', hint: 'Paste a certificate you bought elsewhere.' },
];

export default function Ssl({ me }) {
  const [sites, setSites] = useState([]);
  const [certs, setCerts] = useState([]);
  const [selected, setSelected] = useState(null);
  const [method, setMethod] = useState('le');
  const [reusable, setReusable] = useState(null);
  const [busy, setBusy] = useState(false);
  const msg = useMessage();
  const { ask, dialog } = useConfirm();

  const isAdmin = me.role === 'admin';

  const load = useCallback(async () => {
    try {
      setSites((await api.get('/sites')).sites || []);
    } catch (err) {
      msg.fail(err);
    }
    if (isAdmin) {
      try {
        setCerts((await api.get('/certificates')).certificates || []);
      } catch { /* the list is a convenience, not the page */ }
    }
  }, [isAdmin]); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => { load(); }, [load]);

  useEffect(() => {
    if (!selected) { setReusable(null); return; }
    api.get(`/sites/${selected.id}/certificate/reusable`)
      .then((r) => setReusable(r.certificate))
      .catch(() => setReusable(null));
  }, [selected]);

  async function run(fn, okText) {
    setBusy(true);
    msg.clear();
    try {
      await fn();
      msg.ok(okText);
      setSelected(null);
    } catch (err) {
      msg.fail(err);
    }
    setBusy(false);
    await load();
  }

  return (
    <>
      {dialog}
      <Message value={msg.message} onClear={msg.clear} />

      <Card title="Certificates by website">
        {sites.length === 0 ? (
          <Empty>No websites yet.</Empty>
        ) : (
          <div className="scroll">
            <table>
              <thead>
                <tr><th>Domain</th><th>Status</th><th>Expires</th><th>Redirect</th><th /></tr>
              </thead>
              <tbody>
                {sites.map((s) => {
                  const days = s.cert_expires
                    ? Math.round((new Date(s.cert_expires) - Date.now()) / 86400000)
                    : null;
                  return (
                    <tr key={s.id}>
                      <td><code>{s.domain}</code></td>
                      <td>
                        {s.ssl_enabled
                          ? <Tag kind={days !== null && days < 15 ? 'warn' : 'ok'}>HTTPS on</Tag>
                          : <Tag kind="bad">no certificate</Tag>}
                      </td>
                      <td className="muted">
                        {s.cert_expires ? `${fmtDate(s.cert_expires)} (${days}d)` : '—'}
                      </td>
                      <td>
                        {s.ssl_enabled && (s.force_https
                          ? <Tag>HTTP → HTTPS</Tag>
                          : <Tag kind="warn">HTTP still served</Tag>)}
                      </td>
                      <td className="right nowrap">
                        <button type="button" onClick={() => { setSelected(s); setMethod('le'); }}>
                          {s.ssl_enabled ? 'Replace' : 'Install'}
                        </button>{' '}
                        {s.ssl_enabled && (
                          <button
                            type="button"
                            className="link"
                            onClick={async () => {
                              const ok = await ask({
                                title: `Turn HTTPS off for ${s.domain}?`,
                                body: 'The certificate stays on the server, so turning it '
                                  + 'back on later does not need a new one.',
                                confirmLabel: 'Turn off',
                                danger: true,
                              });
                              if (ok) run(() => api.del(`/sites/${s.id}/certificate`), 'HTTPS turned off');
                            }}
                          >
                            turn off
                          </button>
                        )}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </Card>

      {selected && (
        <Card title={<>Certificate for <code>{selected.domain}</code></>}>
          <div className="row" style={{ marginBottom: '.8rem' }}>
            <div className="field" style={{ flex: '0 0 16rem' }}>
              <label htmlFor="method">How</label>
              <select id="method" value={method} onChange={(e) => setMethod(e.target.value)}>
                {METHODS.map((m) => (
                  <option key={m.id} value={m.id} disabled={m.id === 'reuse' && !reusable}>
                    {m.label}{m.id === 'reuse' && !reusable ? ' — nothing covers this name' : ''}
                  </option>
                ))}
              </select>
            </div>
            <p className="muted" style={{ flex: 1, alignSelf: 'center', fontSize: '.85rem' }}>
              {METHODS.find((m) => m.id === method).hint}
            </p>
            <div><button type="button" onClick={() => setSelected(null)}>Close</button></div>
          </div>

          {method === 'le' && <LetsEncrypt site={selected} busy={busy} run={run} />}
          {method === 'wildcard' && <Wildcard site={selected} busy={busy} run={run} />}
          {method === 'reuse' && reusable && <Reuse site={selected} cert={reusable} busy={busy} run={run} />}
          {method === 'manual' && <Manual site={selected} busy={busy} run={run} />}
        </Card>
      )}

      {isAdmin && (
        <Card title="Certificates on this server">
          {certs.length === 0 ? (
            <Empty>None yet.</Empty>
          ) : (
            <div className="scroll">
              <table>
                <thead>
                  <tr><th>Name</th><th>Covers</th><th>Issuer</th><th>Expires</th><th /></tr>
                </thead>
                <tbody>
                  {certs.map((c) => (
                    <tr key={c.name}>
                      <td>
                        <code>{c.name}</code>
                        {c.wildcard && <> <Tag>wildcard</Tag></>}
                        {c.manual && <> <Tag kind="mute">uploaded</Tag></>}
                      </td>
                      <td className="muted">{(c.domains || []).join(', ')}</td>
                      <td className="muted">{c.issuer}</td>
                      <td className="muted">{fmtDate(c.not_after)}</td>
                      <td className="right">
                        <button
                          type="button"
                          className="link"
                          onClick={async () => {
                            const ok = await ask({
                              title: `Delete the certificate ${c.name}?`,
                              body: 'Refused while a website is still using it.',
                              confirmLabel: 'Delete',
                              danger: true,
                            });
                            if (ok) {
                              run(
                                () => api.del(`/certificates?name=${encodeURIComponent(c.name)}`),
                                'Certificate deleted',
                              );
                            }
                          }}
                        >
                          Delete
                        </button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          <p className="muted" style={{ fontSize: '.83rem', marginBottom: 0 }}>
            Uploaded certificates are never renewed automatically — the panel has
            no way to get a replacement for one you bought.
          </p>
        </Card>
      )}
    </>
  );
}

function ForceHTTPS({ value, onChange }) {
  return (
    <label style={{ display: 'inline', color: 'var(--ink)' }}>
      <input type="checkbox" checked={value} onChange={(e) => onChange(e.target.checked)} />{' '}
      redirect HTTP to HTTPS
    </label>
  );
}

function LetsEncrypt({ site, busy, run }) {
  const [force, setForce] = useState(true);
  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        run(
          () => api.post(`/sites/${site.id}/certificate`, { force_https: force }),
          `HTTPS is on for ${site.domain}`,
        );
      }}
    >
      <p className="muted" style={{ marginTop: 0, fontSize: '.85rem' }}>
        The certificate authority fetches a file over plain HTTP to check that
        you control the name, so <code>{site.domain}</code> has to point at this
        server already. <code>www.{site.domain}</code> is added automatically when
        its DNS sends visitors to the same place.
      </p>
      <ForceHTTPS value={force} onChange={setForce} />
      <p style={{ marginBottom: 0 }}>
        <button type="submit" className="primary" disabled={busy}>
          {busy ? 'Requesting…' : 'Request certificate'}
        </button>
      </p>
    </form>
  );
}

function Wildcard({ site, busy, run }) {
  const base = site.domain.split('.').length > 2
    ? site.domain.split('.').slice(1).join('.')
    : site.domain;
  const [domain, setDomain] = useState(base);
  const [token, setToken] = useState('');
  const [force, setForce] = useState(true);

  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        run(
          () => api.post(`/sites/${site.id}/certificate/wildcard`, {
            domain, cf_token: token, force_https: force,
          }),
          `Wildcard certificate installed for *.${domain}`,
        );
      }}
    >
      <div className="row">
        <div className="field">
          <label htmlFor="wcDomain">Base domain</label>
          <input id="wcDomain" required value={domain} onChange={(e) => setDomain(e.target.value)} />
        </div>
        <div className="field">
          <label htmlFor="wcToken">Cloudflare API token</label>
          <input
            id="wcToken"
            type="password"
            required
            autoComplete="off"
            value={token}
            onChange={(e) => setToken(e.target.value)}
          />
        </div>
      </div>
      <p className="muted" style={{ fontSize: '.85rem' }}>
        Covers <code>{domain}</code> and <code>*.{domain}</code>. Create the token in
        Cloudflare with <strong>Zone → DNS → Edit</strong> on this zone only. The panel
        uses it for this request and does not store it, so renewal of a wildcard
        has to be done here again.
      </p>
      <ForceHTTPS value={force} onChange={setForce} />
      <p style={{ marginBottom: 0 }}>
        <button type="submit" className="primary" disabled={busy}>
          {busy ? 'Waiting for DNS…' : 'Request wildcard'}
        </button>
        {busy && <span className="muted"> This can take several minutes.</span>}
      </p>
    </form>
  );
}

function Reuse({ site, cert, busy, run }) {
  const [force, setForce] = useState(true);
  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        run(
          () => api.post(`/sites/${site.id}/certificate/reuse`, { name: cert.name, force_https: force }),
          `${site.domain} now uses ${cert.name}`,
        );
      }}
    >
      <dl className="kv">
        <dt>Certificate</dt><dd><code>{cert.name}</code></dd>
        <dt>Covers</dt><dd className="muted">{(cert.domains || []).join(', ')}</dd>
        <dt>Expires</dt><dd className="muted">{fmtDate(cert.not_after)}</dd>
      </dl>
      <p className="muted" style={{ fontSize: '.85rem' }}>
        No new order, so this does not touch the rate limit and works
        immediately.
      </p>
      <ForceHTTPS value={force} onChange={setForce} />
      <p style={{ marginBottom: 0 }}>
        <button type="submit" className="primary" disabled={busy}>Use this certificate</button>
      </p>
    </form>
  );
}

function Manual({ site, busy, run }) {
  const [cert, setCert] = useState('');
  const [key, setKey] = useState('');
  const [chain, setChain] = useState('');
  const [force, setForce] = useState(true);

  const area = (label, value, set, required) => (
    <div style={{ marginBottom: '.6rem' }}>
      <label>{label}</label>
      <textarea
        required={required}
        rows={6}
        spellCheck={false}
        style={{ fontFamily: 'ui-monospace, monospace', fontSize: '.8rem' }}
        value={value}
        onChange={(e) => set(e.target.value)}
      />
    </div>
  );

  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        run(
          () => api.post(`/sites/${site.id}/certificate/manual`, {
            certificate: cert, private_key: key, ca_bundle: chain, force_https: force,
          }),
          `Certificate installed for ${site.domain}`,
        );
      }}
    >
      {area('Certificate (PEM)', cert, setCert, true)}
      {area('Private key (PEM)', key, setKey, true)}
      {area('CA bundle (PEM, optional)', chain, setChain, false)}
      <p className="muted" style={{ fontSize: '.85rem' }}>
        Checked before anything is written: that the key matches the
        certificate, that it has not expired, and that it covers{' '}
        <code>{site.domain}</code>. A certificate that fails any of those would
        stop the webserver reloading, so it is refused here instead.
      </p>
      <ForceHTTPS value={force} onChange={setForce} />
      <p style={{ marginBottom: 0 }}>
        <button type="submit" className="primary" disabled={busy}>Install certificate</button>
      </p>
    </form>
  );
}
