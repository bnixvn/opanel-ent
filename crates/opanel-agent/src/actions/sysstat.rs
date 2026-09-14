//! How busy the machine is right now, not a history of it.

use crate::registry::{Failure, Registry};
use serde::Serialize;
use std::thread;
use std::time::Duration;

#[derive(Serialize, Default)]
struct SysStat {
    cpu_percent: f64,
    load1: f64,
    cores: i64,
    mem_total_mb: i64,
    mem_used_mb: i64,
    swap_total_mb: i64,
    swap_used_mb: i64,
    disk_total_mb: i64,
    disk_used_mb: i64,
    uptime_seconds: i64,
}

pub fn register(r: &mut Registry) {
    r.register("sysstat", 1, |_| {
        serde_json::to_value(read()).map_err(|e| Failure::internal(e.to_string()))
    });
}

/// Samples the machine.
///
/// The CPU figure costs a short sleep. /proc/stat counts ticks since boot, so
/// a single read gives the average since the machine started -- a number that
/// stops moving after a week of uptime and would show an idle server as busy
/// for ever. Two reads a fifth of a second apart give the load now, which is
/// the only version of this figure anybody looks at a panel for.
fn read() -> SysStat {
    let mut out = SysStat::default();

    let first = cpu_sample();
    thread::sleep(Duration::from_millis(200));
    let second = cpu_sample();
    if let (Some(a), Some(b)) = (first, second) {
        out.cpu_percent = b.since(a);
    }

    out.cores = cpu_count();
    out.load1 = load_average_1();

    let mem = meminfo();
    let kb = |k: &str| mem.get(k).copied().unwrap_or(0);
    out.mem_total_mb = kb("MemTotal") / 1024;
    if mem.contains_key("MemAvailable") {
        out.mem_used_mb = out.mem_total_mb - kb("MemAvailable") / 1024;
    }
    out.swap_total_mb = kb("SwapTotal") / 1024;
    out.swap_used_mb = out.swap_total_mb - kb("SwapFree") / 1024;

    // The root filesystem. Homes usually live on it; when they do not, this
    // is still the number that decides whether the server keeps working.
    if let Some((total, free)) = disk_usage("/") {
        out.disk_total_mb = (total / (1024 * 1024)) as i64;
        out.disk_used_mb = ((total - free) / (1024 * 1024)) as i64;
    }

    out.uptime_seconds = uptime_seconds();
    out
}

/// The jiffy counter from the first line of /proc/stat.
#[derive(Clone, Copy)]
struct CpuSample {
    idle: u64,
    total: u64,
}

impl CpuSample {
    /// The percentage of the interval that was not idle.
    fn since(self, prev: Self) -> f64 {
        let d_total = self.total.saturating_sub(prev.total) as f64;
        let d_idle = self.idle.saturating_sub(prev.idle) as f64;
        if d_total <= 0.0 {
            return 0.0;
        }
        ((d_total - d_idle) / d_total * 100.0).clamp(0.0, 100.0)
    }
}

fn cpu_sample() -> Option<CpuSample> {
    let body = std::fs::read_to_string("/proc/stat").ok()?;
    let line = body.lines().next()?;
    let mut fields = line.split_whitespace();
    if fields.next()? != "cpu" {
        return None;
    }
    let mut s = CpuSample { idle: 0, total: 0 };
    for (i, f) in fields.enumerate() {
        let Ok(n) = f.parse::<u64>() else { continue };
        s.total += n;
        // Fields 3 and 4 are idle and iowait. A core waiting on a disk is
        // not doing work, and counting it as busy would make every backup
        // look like a runaway process.
        if i == 3 || i == 4 {
            s.idle += n;
        }
    }
    Some(s)
}

fn cpu_count() -> i64 {
    std::fs::read_to_string("/proc/cpuinfo")
        .map(|b| b.lines().filter(|l| l.starts_with("processor")).count() as i64)
        .unwrap_or(0)
}

fn load_average_1() -> f64 {
    std::fs::read_to_string("/proc/loadavg")
        .ok()
        .and_then(|b| b.split_whitespace().next().and_then(|f| f.parse().ok()))
        .unwrap_or(0.0)
}

/// The whole file in kB, keyed without the colon.
fn meminfo() -> std::collections::BTreeMap<String, i64> {
    let mut out = std::collections::BTreeMap::new();
    let Ok(body) = std::fs::read_to_string("/proc/meminfo") else { return out };
    for line in body.lines() {
        let Some((name, rest)) = line.split_once(':') else { continue };
        if let Some(kb) = rest.split_whitespace().next().and_then(|f| f.parse().ok()) {
            out.insert(name.to_owned(), kb);
        }
    }
    out
}

pub fn uptime_seconds() -> i64 {
    std::fs::read_to_string("/proc/uptime")
        .ok()
        .and_then(|b| b.split_whitespace().next().and_then(|f| f.parse::<f64>().ok()))
        .map(|v| v as i64)
        .unwrap_or(0)
}

/// The size of the filesystem holding `path`, and how much of it is free.
///
/// Free rather than available: this is the operator's view of their own
/// server, and the root reserve is space they can actually reach.
fn disk_usage(path: &str) -> Option<(u64, u64)> {
    let c_path = std::ffi::CString::new(path).ok()?;
    let mut st: libc::statfs = unsafe { std::mem::zeroed() };
    // SAFETY: `c_path` is a valid NUL-terminated string that outlives the
    // call, and `st` is the size the kernel expects.
    if unsafe { libc::statfs(c_path.as_ptr(), &raw mut st) } != 0 {
        return None;
    }
    let bs = st.f_bsize as u64;
    Some((st.f_blocks * bs, st.f_bfree * bs))
}
