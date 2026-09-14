import React, { useEffect, useState } from 'react';
import { api, fmtBytes } from './api.js';

// StatusRail is the column down the right of every page.
//
// What it shows depends on who is reading it, because the same figures are
// not true of everybody. An administrator does not host websites under their
// own login, so showing them a personal disk quota describes nothing; what
// they want is the machine and what is on it. A reseller wants what they
// have handed out against what they were allowed. Only an end user has a
// package, and for them it is the only thing on this rail worth reading.

const SERVER_EVERY = 10000;
const USAGE_EVERY = 60000;

export default function StatusRail({ me }) {
  const admin = me.role === 'admin';
  const reseller = me.role === 'reseller';
  const staff = admin || reseller;

  const [stats, setStats] = useState(null);
  const [usage, setUsage] = useState(null);
  const [allowance, setAllowance] = useState(null);

  useEffect(() => {
    if (!staff) return undefined;
    let live = true;
    const tick = () => api.get('/system/stats')
      .then((r) => { if (live) setStats(r); })
      .catch(() => { if (live) setStats(null); });
    tick();
    const id = setInterval(tick, SERVER_EVERY);
    return () => { live = false; clearInterval(id); };
  }, [staff]);

  useEffect(() => {
    if (staff) return undefined;
    let live = true;
    const tick = () => api.get('/usage')
      .then((r) => { if (live) setUsage(r.usage); })
      .catch(() => { if (live) setUsage(null); });
    tick();
    const id = setInterval(tick, USAGE_EVERY);
    return () => { live = false; clearInterval(id); };
  }, [staff]);

  useEffect(() => {
    if (!reseller) return undefined;
    let live = true;
    api.get('/reseller')
      .then((r) => { if (live) setAllowance(r.reseller); })
      .catch(() => { if (live) setAllowance(null); });
    return () => { live = false; };
  }, [reseller]);

  return (
    <aside className="rail">
      {staff && <HostPanel host={stats && stats.host} />}
      {admin && <TotalsPanel totals={stats && stats.totals} />}
      {reseller && <AllowancePanel allowance={allowance} />}
      {!staff && <AccountPanel usage={usage} />}
    </aside>
  );
}

function HostPanel({ host }) {
  if (!host) return <Panel title="Server"><p className="railnote">…</p></Panel>;
  return (
    <Panel title="Server">
      <Meter label="CPU" value={host.cpu_percent} max={100} text={`${Math.round(host.cpu_percent)}%`} />
      <Meter
        label="Memory"
        value={host.mem_used_mb}
        max={host.mem_total_mb}
        text={`${mb(host.mem_used_mb)} / ${mb(host.mem_total_mb)}`}
      />
      <Meter
        label="Disk"
        value={host.disk_used_mb}
        max={host.disk_total_mb}
        text={`${mb(host.disk_used_mb)} / ${mb(host.disk_total_mb)}`}
      />
      {host.swap_total_mb > 0 && (
        <Meter
          label="Swap"
          value={host.swap_used_mb}
          max={host.swap_total_mb}
          text={`${mb(host.swap_used_mb)} / ${mb(host.swap_total_mb)}`}
        />
      )}
      <dl className="railfacts">
        <dt>Load</dt>
        <dd>{host.load1.toFixed(2)}{host.cores > 0 && ` / ${host.cores}`}</dd>
        <dt>Uptime</dt>
        <dd>{uptime(host.uptime_seconds)}</dd>
      </dl>
    </Panel>
  );
}

function TotalsPanel({ totals }) {
  if (!totals) return null;
  return (
    <Panel title="Hosted here">
      <dl className="railfacts">
        <dt>Accounts</dt>
        <dd>{totals.accounts}</dd>
        {totals.resellers > 0 && (
          <>
            <dt>Resellers</dt>
            <dd>{totals.resellers}</dd>
          </>
        )}
        <dt>Websites</dt>
        <dd>{totals.sites}</dd>
        <dt>Databases</dt>
        <dd>{totals.databases}</dd>
      </dl>
    </Panel>
  );
}

function AllowancePanel({ allowance }) {
  if (!allowance) return null;
  const { used, limits } = allowance;
  return (
    <Panel title="Handed out">
      <dl className="railfacts">
        <dt>Accounts</dt>
        <dd>{count(used.accounts, limits.max_accounts)}</dd>
        <dt>Websites</dt>
        <dd>{count(used.sites, limits.max_sites)}</dd>
        <dt>Databases</dt>
        <dd>{count(used.databases, limits.max_databases)}</dd>
        <dt>Disk</dt>
        <dd>
          {limits.disk_quota_mb > 0
            ? `${mb(used.disk_quota_mb)} / ${mb(limits.disk_quota_mb)}`
            : mb(used.disk_quota_mb)}
        </dd>
      </dl>
    </Panel>
  );
}

function AccountPanel({ usage }) {
  if (!usage) return <Panel title="Your account"><p className="railnote">…</p></Panel>;
  return (
    <Panel title={usage.plan_name || 'Your account'}>
      <Meter
        label="Disk"
        value={usage.disk_bytes}
        max={usage.disk_quota_mb * 1024 * 1024}
        text={usage.disk_quota_mb > 0
          ? `${fmtBytes(usage.disk_bytes)} / ${mb(usage.disk_quota_mb)}`
          : fmtBytes(usage.disk_bytes)}
      />
      <dl className="railfacts">
        <dt>Websites</dt>
        <dd>{count(usage.sites, usage.max_sites)}</dd>
        <dt>Databases</dt>
        <dd>{count(usage.databases, usage.max_databases)}</dd>
      </dl>
      {usage.disk_quota_mb > 0 && !usage.quota_enforced && (
        <p className="railnote">Disk is measured, not enforced.</p>
      )}
    </Panel>
  );
}

function Panel({ title, children }) {
  return (
    <section>
      <h2>{title}</h2>
      {children}
    </section>
  );
}

// Meter is a labelled bar. A bar with no ceiling -- an account with no disk
// limit -- would be a bar that is always empty or always full depending on
// which lie you pick, so it shows the figure and no bar at all.
function Meter({ label, value, max, text }) {
  const bounded = max > 0;
  const pct = bounded ? Math.min(100, Math.max(0, (value / max) * 100)) : 0;
  const level = pct >= 90 ? 'bad' : pct >= 75 ? 'warn' : 'ok';
  return (
    <div className="meter">
      <div className="meterhead">
        <span>{label}</span>
        <span>{text}</span>
      </div>
      {bounded && (
        <div className="meterbar">
          <span className={level} style={{ width: `${pct}%` }} />
        </div>
      )}
    </div>
  );
}

function count(used, max) {
  return max > 0 ? `${used} / ${max}` : `${used}`;
}

function mb(n) {
  return fmtBytes((n || 0) * 1024 * 1024);
}

function uptime(seconds) {
  if (!seconds) return '—';
  const d = Math.floor(seconds / 86400);
  const h = Math.floor((seconds % 86400) / 3600);
  if (d > 0) return `${d}d ${h}h`;
  const m = Math.floor((seconds % 3600) / 60);
  return `${h}h ${m}m`;
}
