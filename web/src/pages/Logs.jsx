import React, { useCallback, useEffect, useState } from 'react';
import { api, download, fmtBytes } from '../api.js';
import { Card, Empty, Message, useMessage } from '../components.jsx';

export default function Logs() {
  const [sites, setSites] = useState([]);
  const [site, setSite] = useState('');
  const [kind, setKind] = useState('access');
  const [lines, setLines] = useState(200);
  const [filter, setFilter] = useState('');
  const [result, setResult] = useState(null);
  const [busy, setBusy] = useState(false);
  const msg = useMessage();

  useEffect(() => {
    api.get('/sites').then((r) => {
      const rows = r.sites || [];
      setSites(rows);
      if (rows.length) setSite(String(rows[0].id));
    }).catch(msg.fail);
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  const load = useCallback(async () => {
    if (!site) return;
    setBusy(true);
    msg.clear();
    const params = new URLSearchParams({ site, kind, lines: String(lines) });
    if (filter) params.set('q', filter);
    try {
      setResult(await api.get(`/logs?${params}`));
    } catch (err) {
      msg.fail(err);
      setResult(null);
    }
    setBusy(false);
  }, [site, kind, lines, filter]); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => { load(); }, [site, kind]); // eslint-disable-line react-hooks/exhaustive-deps

  return (
    <>
      <Message value={msg.message} onClear={msg.clear} />

      <Card>
        <form
          className="row"
          onSubmit={(e) => { e.preventDefault(); load(); }}
        >
          <div className="field" style={{ flex: '1 1 16rem' }}>
            <label htmlFor="lgSite">Website</label>
            <select id="lgSite" value={site} onChange={(e) => setSite(e.target.value)}>
              {sites.length === 0 && <option value="">no websites</option>}
              {sites.map((s) => <option key={s.id} value={s.id}>{s.domain}</option>)}
            </select>
          </div>
          <div className="field" style={{ flex: '0 0 9rem' }}>
            <label htmlFor="lgKind">Log</label>
            <select id="lgKind" value={kind} onChange={(e) => setKind(e.target.value)}>
              <option value="access">access</option>
              <option value="error">error</option>
            </select>
          </div>
          <div className="field" style={{ flex: '0 0 7rem' }}>
            <label htmlFor="lgLines">Lines</label>
            <select id="lgLines" value={lines} onChange={(e) => setLines(Number(e.target.value))}>
              {[100, 200, 500, 1000, 5000].map((n) => <option key={n} value={n}>{n}</option>)}
            </select>
          </div>
          <div className="field">
            <label htmlFor="lgFilter">Contains</label>
            <input
              id="lgFilter"
              placeholder="404, an IP, a path…"
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
            />
          </div>
          <div className="nowrap">
            <button type="submit" className="primary" disabled={busy}>
              {busy ? 'Reading…' : 'Show'}
            </button>{' '}
            <button
              type="button"
              disabled={!site}
              onClick={() => download(`/logs/download?site=${site}&kind=${kind}`)}
            >
              Download
            </button>
          </div>
        </form>
        {result && (
          <p className="muted" style={{ margin: '.5rem 0 0', fontSize: '.83rem' }}>
            {result.lines.length} line(s) shown of a {fmtBytes(result.size)} file.
            {result.truncated && ' Older entries are not shown — download the whole log to see them.'}
            {filter && ' Filtering reads the whole file, so it is slower than a plain tail.'}
          </p>
        )}
      </Card>

      <Card title="Output">
        {!result || result.lines.length === 0 ? (
          <Empty>{result ? 'Nothing matched.' : 'Pick a website.'}</Empty>
        ) : (
          <pre
            style={{
              margin: 0,
              maxHeight: '32rem',
              overflow: 'auto',
              fontSize: '.78rem',
              lineHeight: 1.5,
              background: 'var(--bg)',
              padding: '.7rem',
              borderRadius: '6px',
            }}
          >
            {result.lines.join('\n')}
          </pre>
        )}
      </Card>
    </>
  );
}
