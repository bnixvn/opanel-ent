import React, { useEffect, useState } from 'react';
import { Link } from 'react-router-dom';
import { api, fmtBytes } from '../api.js';
import { Card, Tag } from '../components.jsx';

export default function Dashboard({ me }) {
  const [usage, setUsage] = useState(null);
  const [sites, setSites] = useState([]);
  const [reseller, setReseller] = useState(null);

  useEffect(() => {
    api.get('/usage').then((r) => setUsage(r.usage || r)).catch(() => {});
    api.get('/sites').then((r) => setSites(r.sites || [])).catch(() => {});
    if (me.role === 'reseller' || me.role === 'admin') {
      api.get('/reseller').then((r) => setReseller(r.reseller)).catch(() => {});
    }
  }, [me.role]);

  const pct = (used, limit) => (limit > 0 ? Math.round((used / limit) * 100) : null);

  return (
    <>
      <Card title={`Welcome, ${me.username}`}>
        <p className="muted" style={{ marginTop: 0 }}>
          {sites.length === 0
            ? 'No websites yet. Create one to get started.'
            : `${sites.length} website${sites.length === 1 ? '' : 's'}.`}
          {' '}
          <Link to="/sites">Manage websites</Link>
        </p>
      </Card>

      {usage && (
        <Card title="Your usage">
          <dl className="kv">
            <dt>Websites</dt>
            <dd>
              {usage.sites}
              {usage.max_sites > 0 ? ` of ${usage.max_sites}` : ' (unlimited)'}
            </dd>
            <dt>Databases</dt>
            <dd>
              {usage.databases}
              {usage.max_databases > 0 ? ` of ${usage.max_databases}` : ' (unlimited)'}
            </dd>
            <dt>Disk</dt>
            <dd>
              {fmtBytes(usage.disk_bytes)}
              {usage.disk_quota_mb > 0 ? ` of ${usage.disk_quota_mb} MB` : ' (unlimited)'}
              {usage.disk_quota_mb > 0 && (
                <>
                  {' '}
                  <Tag kind={pct(usage.disk_bytes / 1048576, usage.disk_quota_mb) >= 90 ? 'bad' : 'mute'}>
                    {pct(usage.disk_bytes / 1048576, usage.disk_quota_mb)}%
                  </Tag>
                </>
              )}
              {/* Saying which kind of limit this is matters: an advisory
                  number and an enforced one call for different actions. */}
              {!usage.quota_enforced && usage.disk_quota_mb > 0 && (
                <div className="muted" style={{ fontSize: '.8rem' }}>
                  Not enforced by the filesystem on this server.
                </div>
              )}
            </dd>
            {usage.plan_name && (
              <>
                <dt>Package</dt>
                <dd>{usage.plan_name}</dd>
              </>
            )}
          </dl>
        </Card>
      )}

      {reseller && (
        <Card title="Your reseller allowance">
          <dl className="kv">
            <dt>Accounts</dt>
            <dd>
              {reseller.used.accounts}
              {reseller.limits.max_accounts > 0 ? ` of ${reseller.limits.max_accounts}` : ' (unlimited)'}
            </dd>
            <dt>Disk allocated</dt>
            <dd>
              {reseller.used.disk_quota_mb} MB
              {reseller.limits.disk_quota_mb > 0
                ? ` of ${reseller.limits.disk_quota_mb} MB`
                : ' (unlimited)'}
              {reseller.limits.allow_oversell && (
                <>
                  {' '}
                  <Tag kind="warn">overselling allowed</Tag>
                </>
              )}
            </dd>
            <dt>Websites</dt>
            <dd>{reseller.used.sites}</dd>
            <dt>Databases</dt>
            <dd>{reseller.used.databases}</dd>
          </dl>
          <p className="muted" style={{ fontSize: '.83rem', marginBottom: 0 }}>
            Disk is counted as what you have promised your customers, not what
            they are using.
          </p>
        </Card>
      )}
    </>
  );
}
