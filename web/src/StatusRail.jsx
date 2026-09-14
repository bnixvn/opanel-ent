import React, { useEffect, useState } from 'react';
import { api, fmtBytes } from './api.js';

// StatusRail is the column down the right of every page.
//
// Two questions it answers without anybody navigating anywhere: is the
// machine in trouble, and how much of what I am paying for have I used. The
// server half is staff-only, because on a shared box the load is mostly made
// of other tenants' traffic.

const SERVER_EVERY = 10000;
const USAGE_EVERY = 60000;

export default function StatusRail({ me }) {
  const staff = me.role === 'admin' || me.role === 'reseller';
  const [stats, setStats] = useState(null);
  const [usage, setUsage] = useState(null);

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
    let live = true;
    const tick = () => api.get('/usage')
      .then((r) => { if (live) setUsage(r.usage); })
      .catch(() => { if (live) setUsage(null); });
    tick();
    const id = setInterval(tick, USAGE_EVERY);
    return () => { live = false; clearInterval(id); };
  }, []);

  return (
    <aside className="rail">
      {staff && <ServerPanel stats={stats} />}
      <AccountPanel me={me} usage={usage} />
    </aside>
  );
}

function ServerPanel({ stats }) {
  return (
    <section>
      <h2>Server</h2>
      {!stats ? (
        <p className="railnote">…</p>
      ) : (
        <>
          <Meter
            label="CPU"
            value={stats.cpu_percent}
            max={100}
            text={`${Math.round(stats.cpu_percent)}%`}
          />
          <Meter
            label="Memory"
            value={stats.mem_used_mb}
            max={stats.mem_total_mb}
            text={`${mb(stats.mem_used_mb)} / ${mb(stats.mem_total_mb)}`}
          />
          <Meter
            label="Disk"
            value={stats.disk_used_mb}
            max={stats.disk_total_mb}
            text={`${mb(stats.disk_used_mb)} / ${mb(stats.disk_total_mb)}`}
          />
          {stats.swap_total_mb > 0 && (
            <Meter
              label="Swap"
              value={stats.swap_used_mb}
              max={stats.swap_total_mb}
              text={`${mb(stats.swap_used_mb)} / ${mb(stats.swap_total_mb)}`}
            />
          )}
          <dl className="railfacts">
            <dt>Load</dt>
            <dd>
              {stats.load1.toFixed(2)}
              {stats.cores > 0 && ` / ${stats.cores}`}
            </dd>
            <dt>Uptime</dt>
            <dd>{uptime(stats.uptime_seconds)}</dd>
          </dl>
        </>
      )}
    </section>
  );
}

function AccountPanel({ me, usage }) {
  return (
    <section>
      <h2>{me.username}</h2>
      {!usage ? (
        <p className="railnote">…</p>
      ) : (
        <>
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
            {usage.plan_name && (
              <>
                <dt>Package</dt>
                <dd>{usage.plan_name}</dd>
              </>
            )}
          </dl>
          {usage.disk_quota_mb > 0 && !usage.quota_enforced && (
            <p className="railnote">Disk is measured, not enforced.</p>
          )}
        </>
      )}
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
