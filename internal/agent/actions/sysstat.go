package actions

import (
	"bufio"
	"context"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bnixvn/opanel-ent/internal/agent"
)

// SysStatResult is what the panel's status panel shows: how busy the machine
// is right now, not a history of it.
type SysStatResult struct {
	CPUPercent float64 `json:"cpu_percent"`
	Load1      float64 `json:"load1"`
	Cores      int     `json:"cores"`

	MemTotalMB int `json:"mem_total_mb"`
	MemUsedMB  int `json:"mem_used_mb"`

	SwapTotalMB int `json:"swap_total_mb"`
	SwapUsedMB  int `json:"swap_used_mb"`

	DiskTotalMB int `json:"disk_total_mb"`
	DiskUsedMB  int `json:"disk_used_mb"`

	UptimeSeconds int64 `json:"uptime_seconds"`
}

func registerSysStat(r *agent.Registry) {
	agent.Register(r, "sysstat", 1, func(ctx context.Context, _ struct{}) (SysStatResult, error) {
		return readSysStat(ctx)
	})
}

// readSysStat samples the machine.
//
// The CPU figure costs a short sleep. /proc/stat counts ticks since boot, so
// a single read gives the average since the machine started -- a number that
// stops moving after a week of uptime and would show an idle server as busy
// for ever. Two reads a fifth of a second apart give the load now, which is
// the only version of this figure anybody looks at a panel for.
func readSysStat(ctx context.Context) (SysStatResult, error) {
	var out SysStatResult

	first, ok1 := readCPUSample()
	select {
	case <-ctx.Done():
		return out, ctx.Err()
	case <-time.After(200 * time.Millisecond):
	}
	second, ok2 := readCPUSample()
	if ok1 && ok2 {
		out.CPUPercent = second.since(first)
	}

	out.Cores = cpuCount()
	out.Load1 = loadAverage1()

	mem := readMeminfo()
	out.MemTotalMB = mem["MemTotal"] / 1024
	if avail, hit := mem["MemAvailable"]; hit {
		out.MemUsedMB = out.MemTotalMB - avail/1024
	}
	out.SwapTotalMB = mem["SwapTotal"] / 1024
	out.SwapUsedMB = out.SwapTotalMB - mem["SwapFree"]/1024

	// The root filesystem. Homes usually live on it; when they do not, this
	// is still the number that decides whether the server keeps working.
	if total, free, err := diskUsage("/"); err == nil {
		out.DiskTotalMB = int(total / (1024 * 1024))
		out.DiskUsedMB = int((total - free) / (1024 * 1024))
	}

	out.UptimeSeconds = uptimeSeconds()
	return out, nil
}

// cpuSample is the jiffy counter from the first line of /proc/stat.
type cpuSample struct {
	idle  uint64
	total uint64
}

// since reports the percentage of the interval that was not idle.
func (c cpuSample) since(prev cpuSample) float64 {
	dTotal := float64(c.total - prev.total)
	dIdle := float64(c.idle - prev.idle)
	if dTotal <= 0 {
		return 0
	}
	pct := (dTotal - dIdle) / dTotal * 100
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		return 100
	}
	return pct
}

func readCPUSample() (cpuSample, bool) {
	body, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuSample{}, false
	}
	line, _, _ := strings.Cut(string(body), "\n")
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuSample{}, false
	}
	var s cpuSample
	for i, f := range fields[1:] {
		n, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			continue
		}
		s.total += n
		// Fields 3 and 4 are idle and iowait. A core waiting on a disk is
		// not doing work, and counting it as busy would make every backup
		// look like a runaway process.
		if i == 3 || i == 4 {
			s.idle += n
		}
	}
	return s, true
}

func cpuCount() int {
	body, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "processor") {
			n++
		}
	}
	return n
}

func loadAverage1() float64 {
	body, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(body))
	if len(fields) == 0 {
		return 0
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return v
}

// readMeminfo returns the whole file in kB, keyed without the colon.
func readMeminfo() map[string]int {
	out := map[string]int{}
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return out
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, rest, found := strings.Cut(sc.Text(), ":")
		if !found {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		if kb, err := strconv.Atoi(fields[0]); err == nil {
			out[name] = kb
		}
	}
	return out
}

func uptimeSeconds() int64 {
	body, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	first, _, _ := strings.Cut(string(body), " ")
	v, err := strconv.ParseFloat(first, 64)
	if err != nil {
		return 0
	}
	return int64(v)
}
