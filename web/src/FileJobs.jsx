import React, { useCallback, useEffect, useRef, useState } from 'react';
import { api, fmtDate } from './api.js';

// Packing, unpacking and saving an upload happen on the server after the
// request that asked for them has been answered. This is how a page finds
// out how they went, and why closing the tab no longer stops the work.

const POLL_MS = 2000;

export function useFileJobs(ownerQuery, onFinished) {
  const [jobs, setJobs] = useState([]);
  // Kept in a ref so a change of callback does not restart the poll, and so
  // the poll can read the latest one without being re-created.
  const finished = useRef(onFinished);
  finished.current = onFinished;

  const watching = jobs.some((j) => j.status === 'running');

  const load = useCallback(async () => {
    try {
      const res = await api.get(`/file-jobs?${ownerQuery}&limit=8`);
      setJobs((before) => {
        const after = res.jobs || [];
        // Anything that was running and is not any more is worth a reload of
        // whatever the page is showing: the files on disk have changed.
        const wasRunning = new Set(
          before.filter((j) => j.status === 'running').map((j) => j.id),
        );
        const justDone = after.filter(
          (j) => wasRunning.has(j.id) && j.status !== 'running',
        );
        if (justDone.length && finished.current) finished.current(justDone);
        return after;
      });
    } catch {
      // A failed poll is not worth an error banner; the next one may work.
    }
  }, [ownerQuery]);

  useEffect(() => { load(); }, [load]);

  // Poll only while something is running. A page nobody is waiting on should
  // not be asking the server a question every two seconds for ever.
  useEffect(() => {
    if (!watching) return undefined;
    const id = setInterval(load, POLL_MS);
    return () => clearInterval(id);
  }, [watching, load]);

  return { jobs, refresh: load };
}

// JobList shows what is running and what finished recently. Anything older
// than the last few is not interesting enough to keep on screen.
export function JobList({ jobs }) {
  const shown = (jobs || []).filter(
    (j) => j.status === 'running' || j.status === 'failed' || recent(j),
  );
  if (shown.length === 0) return null;

  return (
    <div className="jobs">
      {shown.map((j) => (
        <div key={j.id} className={`job ${j.status}`}>
          <span className="jobkind">{label(j.kind)}</span>
          <span className="jobtarget">{j.target}</span>
          <span className="spacer" />
          <span className="jobstate">
            {j.status === 'running' ? 'working…'
              : j.status === 'failed' ? j.error || 'failed'
                : j.detail || 'done'}
          </span>
          {j.finished_at && (
            <span className="muted jobtime">{fmtDate(j.finished_at)}</span>
          )}
        </div>
      ))}
    </div>
  );
}

function label(kind) {
  switch (kind) {
    case 'archive': return 'Compressing';
    case 'extract': return 'Extracting';
    case 'upload': return 'Saving';
    default: return kind;
  }
}

// recent keeps a finished job on screen for a minute, long enough to read.
function recent(job) {
  if (!job.finished_at) return false;
  const at = Date.parse(job.finished_at);
  return Number.isFinite(at) && Date.now() - at < 60000;
}
