// ops-gateway is a tiny authenticated HTTP service for out-of-band
// management of the OpenClaw gateway. It exposes status/restart/doctor endpoints
// protected by either a bearer token or Cloudflare Access (Zero Trust).
//
// Background health checker runs every 30s and caches results for the UI.
package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// --- Gateway types ---

type response struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	Time    string `json:"time"`
}

type healthCheck struct {
	OK        bool   `json:"ok"`
	Message   string `json:"message"`
	Timestamp string `json:"timestamp"`
}

type statusResponse struct {
	Current healthCheck   `json:"current"`
	History []healthCheck `json:"history"`
	Uptime  string        `json:"uptime"`
}

type doctorResponse struct {
	OK     bool   `json:"ok"`
	Output string `json:"output"`
	Time   string `json:"time"`
}

type healthResponse struct {
	OK      bool         `json:"ok"`
	Message string       `json:"message"`
	Time    string       `json:"time"`
	Cron    *cronSummary `json:"cron,omitempty"`
}

// --- Cron types ---

type cronSchedule struct {
	Kind    string `json:"kind"`
	EveryMs int64  `json:"everyMs,omitempty"`
	Expr    string `json:"expr,omitempty"`
	Tz      string `json:"tz,omitempty"`
}

type cronJobDef struct {
	ID       string       `json:"id"`
	Name     string       `json:"name"`
	Enabled  bool         `json:"enabled"`
	Schedule cronSchedule `json:"schedule"`
	AgentID  string       `json:"agentId,omitempty"`
}

type cronJobsFile struct {
	Version int          `json:"version"`
	Jobs    []cronJobDef `json:"jobs"`
}

type cronJobState struct {
	LastRunAtMs       int64  `json:"lastRunAtMs"`
	LastRunStatus     string `json:"lastRunStatus"`
	LastDurationMs    int64  `json:"lastDurationMs"`
	NextRunAtMs       int64  `json:"nextRunAtMs"`
	ConsecutiveErrors int    `json:"consecutiveErrors"`
	LastError         string `json:"lastError"`
}

type cronStateEntry struct {
	UpdatedAtMs int64        `json:"updatedAtMs"`
	State       cronJobState `json:"state"`
}

type cronStateFile struct {
	Version int                       `json:"version"`
	Jobs    map[string]cronStateEntry `json:"jobs"`
}

type cronJobInfo struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Enabled           bool   `json:"enabled"`
	Schedule          string `json:"schedule"`
	AgentID           string `json:"agentId,omitempty"`
	LastRunAt         string `json:"lastRunAt,omitempty"`
	LastRunAgo        string `json:"lastRunAgo,omitempty"`
	LastStatus        string `json:"lastStatus"`
	LastDurationMs    int64  `json:"lastDurationMs"`
	NextRunAt         string `json:"nextRunAt,omitempty"`
	NextRunIn         string `json:"nextRunIn,omitempty"`
	ConsecutiveErrors int    `json:"consecutiveErrors"`
	LastError         string `json:"lastError,omitempty"`
}

type cronSummary struct {
	Total    int `json:"total"`
	Enabled  int `json:"enabled"`
	OK       int `json:"ok"`
	Errored  int `json:"errored"`
	Disabled int `json:"disabled"`
}

type cronListResponse struct {
	Jobs    []cronJobInfo `json:"jobs"`
	Summary cronSummary   `json:"summary"`
}

type cronRunResponse struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	Output  string `json:"output,omitempty"`
	JobID   string `json:"jobId"`
	Time    string `json:"time"`
}

// --- Health cache ---

type healthCache struct {
	mu      sync.RWMutex
	checks  []healthCheck
	maxSize int
}

func newHealthCache(size int) *healthCache {
	return &healthCache{checks: make([]healthCheck, 0, size), maxSize: size}
}

func (c *healthCache) add(check healthCheck) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checks = append(c.checks, check)
	if len(c.checks) > c.maxSize {
		c.checks = c.checks[len(c.checks)-c.maxSize:]
	}
}

func (c *healthCache) latest() (healthCheck, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.checks) == 0 {
		return healthCheck{}, false
	}
	return c.checks[len(c.checks)-1], true
}

func (c *healthCache) all() []healthCheck {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make([]healthCheck, len(c.checks))
	copy(result, c.checks)
	return result
}

// --- Health & cron functions ---

func checkGatewayHealth() healthCheck {
	cmd := exec.Command("systemctl", "--user", "is-active", "openclaw-gateway")
	output, _ := cmd.Output()
	status := strings.TrimSpace(string(output))
	return healthCheck{
		OK:        status == "active",
		Message:   status,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
}

func readCronData() (cronListResponse, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return cronListResponse{}, err
	}

	cronDir := filepath.Join(home, ".openclaw", "cron")

	jobsData, err := os.ReadFile(filepath.Join(cronDir, "jobs.json"))
	if err != nil {
		return cronListResponse{}, fmt.Errorf("read jobs.json: %w", err)
	}
	var jobsFile cronJobsFile
	if err := json.Unmarshal(jobsData, &jobsFile); err != nil {
		return cronListResponse{}, fmt.Errorf("parse jobs.json: %w", err)
	}

	var stateFile cronStateFile
	stateData, err := os.ReadFile(filepath.Join(cronDir, "jobs-state.json"))
	if err == nil {
		json.Unmarshal(stateData, &stateFile)
	}
	if stateFile.Jobs == nil {
		stateFile.Jobs = make(map[string]cronStateEntry)
	}

	now := time.Now()
	var jobs []cronJobInfo
	summary := cronSummary{Total: len(jobsFile.Jobs)}

	for _, j := range jobsFile.Jobs {
		info := cronJobInfo{
			ID:       j.ID,
			Name:     j.Name,
			Enabled:  j.Enabled,
			Schedule: formatSchedule(j.Schedule),
			AgentID:  j.AgentID,
		}

		if !j.Enabled {
			summary.Disabled++
		} else {
			summary.Enabled++
		}

		if st, ok := stateFile.Jobs[j.ID]; ok {
			s := st.State
			if s.LastRunAtMs > 0 {
				t := time.UnixMilli(s.LastRunAtMs)
				info.LastRunAt = t.UTC().Format(time.RFC3339)
				info.LastRunAgo = relativeTime(now, t)
			}
			info.LastStatus = s.LastRunStatus
			info.LastDurationMs = s.LastDurationMs
			info.ConsecutiveErrors = s.ConsecutiveErrors
			info.LastError = s.LastError

			if s.NextRunAtMs > 0 {
				t := time.UnixMilli(s.NextRunAtMs)
				info.NextRunAt = t.UTC().Format(time.RFC3339)
				if t.After(now) {
					info.NextRunIn = "in " + relativeTime(t, now)
				} else {
					info.NextRunIn = relativeTime(now, t) + " overdue"
				}
			}

			if j.Enabled {
				if s.LastRunStatus == "ok" {
					summary.OK++
				} else if s.LastRunStatus == "error" {
					summary.Errored++
				}
			}
		} else {
			info.LastStatus = "never"
		}

		jobs = append(jobs, info)
	}

	return cronListResponse{Jobs: jobs, Summary: summary}, nil
}

func formatSchedule(s cronSchedule) string {
	switch s.Kind {
	case "every":
		ms := s.EveryMs
		switch {
		case ms >= 86400000:
			return fmt.Sprintf("every %dd", ms/86400000)
		case ms >= 3600000:
			return fmt.Sprintf("every %dh", ms/3600000)
		case ms >= 60000:
			return fmt.Sprintf("every %dm", ms/60000)
		default:
			return fmt.Sprintf("every %ds", ms/1000)
		}
	case "cron":
		tz := s.Tz
		if tz == "" {
			tz = "UTC"
		}
		return fmt.Sprintf("%s (%s)", s.Expr, tz)
	case "at":
		return "one-time"
	default:
		return s.Kind
	}
}

func relativeTime(a, b time.Time) string {
	d := a.Sub(b)
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m > 0 {
			return fmt.Sprintf("%dh %dm", h, m)
		}
		return fmt.Sprintf("%dh", h)
	default:
		days := int(d.Hours()) / 24
		h := int(d.Hours()) % 24
		if h > 0 {
			return fmt.Sprintf("%dd %dh", days, h)
		}
		return fmt.Sprintf("%dd", days)
	}
}

func formatDuration(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h > 24 {
		days := h / 24
		h = h % 24
		return fmt.Sprintf("%dd %dh %dm", days, h, m)
	}
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

func formatDurationMs(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	s := float64(ms) / 1000
	if s < 60 {
		return fmt.Sprintf("%.1fs", s)
	}
	m := int(s) / 60
	sec := int(s) % 60
	return fmt.Sprintf("%dm %ds", m, sec)
}

// --- UI HTML ---

const uiHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>OpenClaw Ops</title>
<style>
  * { margin: 0; padding: 0; box-sizing: border-box; }
  body {
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
    background: #0a0e17; color: #e2e8f0;
    display: flex; flex-direction: column; align-items: center;
    min-height: 100vh; padding: 2rem 1rem;
  }
  .container { max-width: 580px; width: 100%; }
  .card {
    background: #1a1f2e; border-radius: 12px; padding: 2rem;
    width: 100%; box-shadow: 0 4px 24px rgba(0,0,0,0.4);
  }
  h1 { font-size: 1.4rem; margin-bottom: 1.5rem; text-align: center; }
  .status-row {
    display: flex; justify-content: space-between; align-items: center;
    padding: 0.75rem 0; border-bottom: 1px solid #2d3548;
  }
  .status-dot {
    width: 12px; height: 12px; border-radius: 50%;
    display: inline-block; margin-right: 0.5rem;
  }
  .dot-green { background: #22c55e; box-shadow: 0 0 8px #22c55e80; }
  .dot-red { background: #ef4444; box-shadow: 0 0 8px #ef444480; }
  .dot-gray { background: #6b7280; }
  .dot-yellow { background: #f59e0b; box-shadow: 0 0 8px #f59e0b80; }
  .status-label { display: flex; align-items: center; }
  .status-value { color: #94a3b8; font-size: 0.9rem; }
  .btn {
    display: block; width: 100%; padding: 0.85rem;
    margin-top: 0.75rem; border: none; border-radius: 8px;
    font-size: 1rem; font-weight: 600; cursor: pointer;
    transition: all 0.2s;
  }
  .btn-restart { background: #3b82f6; color: white; }
  .btn-restart:hover { background: #2563eb; }
  .btn-restart:active { transform: scale(0.98); }
  .btn-restart:disabled { background: #475569; cursor: not-allowed; }
  .btn-row { display: flex; gap: 0.5rem; margin-top: 0.75rem; }
  .btn-sm {
    flex: 1; padding: 0.6rem; border: none; border-radius: 8px;
    font-size: 0.85rem; font-weight: 600; cursor: pointer;
    transition: all 0.2s;
  }
  .btn-doctor { background: #6366f1; color: white; }
  .btn-doctor:hover { background: #4f46e5; }
  .btn-fix { background: #f59e0b; color: #1a1f2e; }
  .btn-fix:hover { background: #d97706; }
  .btn-sm:disabled { background: #475569; color: #94a3b8; cursor: not-allowed; }
  .msg {
    text-align: center; margin-top: 1rem; font-size: 0.9rem;
    min-height: 1.2rem; color: #94a3b8;
  }
  .msg-ok { color: #22c55e; }
  .msg-err { color: #ef4444; }
  .history { margin-top: 1.5rem; }
  .history h2 { font-size: 0.85rem; color: #64748b; margin-bottom: 0.5rem; text-transform: uppercase; letter-spacing: 0.05em; }
  .history-bar { display: flex; gap: 2px; height: 24px; align-items: flex-end; }
  .history-pip { flex: 1; border-radius: 2px; min-width: 4px; height: 100%; transition: background 0.3s; }
  .pip-green { background: #22c55e; }
  .pip-red { background: #ef4444; }
  .pip-gray { background: #2d3548; }
  .history-meta { display: flex; justify-content: space-between; font-size: 0.7rem; color: #475569; margin-top: 0.25rem; }
  .uptime-text { text-align: center; color: #64748b; font-size: 0.8rem; margin-top: 0.5rem; }
  .output-box {
    margin-top: 1rem; background: #0f1219; border-radius: 8px;
    padding: 0.75rem; font-family: monospace; font-size: 0.75rem;
    color: #94a3b8; max-height: 200px; overflow-y: auto;
    white-space: pre-wrap; word-break: break-word; display: none;
  }

  /* Cron section */
  .cron-card {
    background: #1a1f2e; border-radius: 12px; padding: 1.5rem 2rem;
    width: 100%; box-shadow: 0 4px 24px rgba(0,0,0,0.4);
    margin-top: 1rem;
  }
  .cron-header {
    display: flex; justify-content: space-between; align-items: center;
    margin-bottom: 1rem;
  }
  .cron-header h2 {
    font-size: 1.1rem; font-weight: 600;
  }
  .cron-summary {
    display: flex; gap: 0.75rem; font-size: 0.8rem; color: #94a3b8;
  }
  .cron-summary .count { font-weight: 600; }
  .cron-summary .ok { color: #22c55e; }
  .cron-summary .err { color: #ef4444; }
  .cron-summary .off { color: #6b7280; }
  .cron-list { display: flex; flex-direction: column; gap: 0; }
  .cron-job {
    display: grid;
    grid-template-columns: 14px 1fr auto;
    gap: 0.75rem;
    align-items: center;
    padding: 0.65rem 0;
    border-bottom: 1px solid #2d354830;
  }
  .cron-job:last-child { border-bottom: none; }
  .cron-dot { width: 10px; height: 10px; border-radius: 50%; }
  .cron-info { min-width: 0; }
  .cron-name {
    font-size: 0.9rem; font-weight: 500;
    white-space: nowrap; overflow: hidden; text-overflow: ellipsis;
  }
  .cron-name.disabled { color: #64748b; }
  .cron-meta {
    font-size: 0.75rem; color: #64748b; margin-top: 0.15rem;
    display: flex; flex-wrap: wrap; gap: 0.25rem 0.75rem;
  }
  .cron-meta .err-text { color: #ef4444; }
  .cron-actions { display: flex; align-items: center; }
  .btn-run {
    padding: 0.35rem 0.7rem; border: none; border-radius: 6px;
    font-size: 0.75rem; font-weight: 600; cursor: pointer;
    background: #22c55e20; color: #22c55e; border: 1px solid #22c55e40;
    transition: all 0.2s; white-space: nowrap;
  }
  .btn-run:hover { background: #22c55e30; }
  .btn-run:disabled { background: #47556920; color: #475569; border-color: #47556940; cursor: not-allowed; }
  .btn-run.running { background: #3b82f620; color: #3b82f6; border-color: #3b82f640; }
  .cron-loading { text-align: center; color: #64748b; padding: 1rem; font-size: 0.85rem; }
  .cron-error { text-align: center; color: #ef4444; padding: 1rem; font-size: 0.85rem; }
</style>
</head>
<body>
<div class="container">
  <div class="card">
    <h1>🤔 OpenClaw Ops</h1>
    <div class="status-row">
      <span class="status-label">
        <span id="gw-dot" class="status-dot dot-gray"></span>
        Gateway
      </span>
      <span id="gw-status" class="status-value">checking...</span>
    </div>
    <div class="status-row" style="border-bottom:none;">
      <span class="status-label">
        <span class="status-dot dot-green"></span>
        Ops Gateway
      </span>
      <span class="status-value">active</span>
    </div>

    <div class="history">
      <h2>Last 30 minutes</h2>
      <div id="history-bar" class="history-bar"></div>
      <div class="history-meta"><span>30m ago</span><span>now</span></div>
      <div id="uptime-text" class="uptime-text"></div>
    </div>

    <button id="restart-btn" class="btn btn-restart" onclick="doRestart()">Restart Gateway</button>
    <div class="btn-row">
      <button id="doctor-btn" class="btn-sm btn-doctor" onclick="doDoctor(false)">Doctor</button>
      <button id="fix-btn" class="btn-sm btn-fix" onclick="doDoctor(true)">Doctor --fix</button>
    </div>
    <div id="msg" class="msg"></div>
    <pre id="output" class="output-box"></pre>
  </div>

  <div class="cron-card">
    <div class="cron-header">
      <h2>Cron Jobs</h2>
      <div id="cron-summary" class="cron-summary"></div>
    </div>
    <div id="cron-list" class="cron-list">
      <div class="cron-loading">Loading...</div>
    </div>
  </div>
</div>

<script>
async function checkStatus() {
  try {
    const r = await fetch('/status');
    const d = await r.json();
    const dot = document.getElementById('gw-dot');
    const txt = document.getElementById('gw-status');
    if (d.current.ok) {
      dot.className = 'status-dot dot-green';
      txt.textContent = 'active';
    } else {
      dot.className = 'status-dot dot-red';
      txt.textContent = d.current.message || 'inactive';
    }
    renderHistory(d.history || []);
    if (d.uptime) {
      document.getElementById('uptime-text').textContent = 'Uptime: ' + d.uptime;
    }
  } catch(e) {
    document.getElementById('gw-dot').className = 'status-dot dot-red';
    document.getElementById('gw-status').textContent = 'error';
  }
}

function renderHistory(checks) {
  const bar = document.getElementById('history-bar');
  const slots = 60;
  let html = '';
  const start = Math.max(0, checks.length - slots);
  const visible = checks.slice(start);
  for (let i = 0; i < slots - visible.length; i++) {
    html += '<div class="history-pip pip-gray"></div>';
  }
  for (const c of visible) {
    html += '<div class="history-pip ' + (c.ok ? 'pip-green' : 'pip-red') + '"></div>';
  }
  bar.innerHTML = html;
}

async function doRestart() {
  const btn = document.getElementById('restart-btn');
  const msg = document.getElementById('msg');
  const out = document.getElementById('output');
  btn.disabled = true;
  btn.textContent = 'Restarting...';
  msg.textContent = '';
  msg.className = 'msg';
  out.style.display = 'none';
  try {
    const r = await fetch('/restart', {method:'POST'});
    const d = await r.json();
    if (d.ok) {
      msg.textContent = '\u2705 Restarted successfully';
      msg.className = 'msg msg-ok';
      setTimeout(checkStatus, 5000);
    } else {
      msg.textContent = '\u274c ' + d.message;
      msg.className = 'msg msg-err';
    }
  } catch(e) {
    msg.textContent = '\u274c Request failed';
    msg.className = 'msg msg-err';
  }
  btn.disabled = false;
  btn.textContent = 'Restart Gateway';
}

async function doDoctor(fix) {
  const btn = fix ? document.getElementById('fix-btn') : document.getElementById('doctor-btn');
  const msg = document.getElementById('msg');
  const out = document.getElementById('output');
  btn.disabled = true;
  btn.textContent = fix ? 'Fixing...' : 'Running...';
  msg.textContent = '';
  msg.className = 'msg';
  out.style.display = 'none';
  try {
    const url = fix ? '/doctor?fix=true' : '/doctor';
    const r = await fetch(url, {method:'POST'});
    const d = await r.json();
    if (d.ok) {
      msg.textContent = '\u2705 ' + (fix ? 'Doctor --fix complete' : 'Doctor complete');
      msg.className = 'msg msg-ok';
    } else {
      msg.textContent = '\u26a0\ufe0f Issues found';
      msg.className = 'msg msg-err';
    }
    if (d.output) {
      out.textContent = d.output;
      out.style.display = 'block';
    }
    if (fix) setTimeout(checkStatus, 3000);
  } catch(e) {
    msg.textContent = '\u274c Request failed';
    msg.className = 'msg msg-err';
  }
  btn.disabled = false;
  btn.textContent = fix ? 'Doctor --fix' : 'Doctor';
}

// --- Cron ---

const runningJobs = new Set();

async function loadCron() {
  const list = document.getElementById('cron-list');
  const summary = document.getElementById('cron-summary');
  try {
    const r = await fetch('/cron');
    const d = await r.json();
    renderCronSummary(d.summary, summary);
    renderCronJobs(d.jobs, list);
  } catch(e) {
    list.innerHTML = '<div class="cron-error">Failed to load cron data</div>';
  }
}

function renderCronSummary(s, el) {
  el.innerHTML =
    '<span><span class="count ok">' + s.ok + '</span> ok</span>' +
    (s.errored > 0 ? '<span><span class="count err">' + s.errored + '</span> err</span>' : '') +
    '<span><span class="count off">' + s.disabled + '</span> off</span>';
}

function dotClass(job) {
  if (!job.enabled) return 'cron-dot dot-gray';
  if (job.lastStatus === 'ok') return 'cron-dot dot-green';
  if (job.lastStatus === 'error') return 'cron-dot dot-red';
  if (job.lastStatus === 'never') return 'cron-dot dot-yellow';
  return 'cron-dot dot-gray';
}

function renderCronJobs(jobs, container) {
  if (!jobs || jobs.length === 0) {
    container.innerHTML = '<div class="cron-loading">No cron jobs configured</div>';
    return;
  }
  let html = '';
  for (const j of jobs) {
    const isRunning = runningJobs.has(j.id);
    const nameClass = j.enabled ? 'cron-name' : 'cron-name disabled';

    let meta = '<span>' + esc(j.schedule) + '</span>';
    if (j.lastRunAgo) {
      meta += '<span>ran ' + esc(j.lastRunAgo) + ' ago</span>';
    }
    if (j.lastDurationMs > 0) {
      meta += '<span>' + fmtDuration(j.lastDurationMs) + '</span>';
    }
    if (j.nextRunIn) {
      meta += '<span>next ' + esc(j.nextRunIn) + '</span>';
    }
    if (j.consecutiveErrors > 0) {
      meta += '<span class="err-text">' + j.consecutiveErrors + ' consecutive errors</span>';
    }
    if (j.lastError) {
      meta += '<span class="err-text">' + esc(j.lastError) + '</span>';
    }

    const btnLabel = isRunning ? 'Running…' : 'Run';
    const btnClass = isRunning ? 'btn-run running' : 'btn-run';
    const btnDisabled = (!j.enabled || isRunning) ? ' disabled' : '';

    html += '<div class="cron-job">' +
      '<span class="' + dotClass(j) + '"></span>' +
      '<div class="cron-info">' +
        '<div class="' + nameClass + '">' + esc(j.name || j.id.slice(0,8)) + '</div>' +
        '<div class="cron-meta">' + meta + '</div>' +
      '</div>' +
      '<div class="cron-actions">' +
        '<button class="' + btnClass + '"' + btnDisabled +
        ' onclick="runJob(\'' + j.id + '\', this)">' + btnLabel + '</button>' +
      '</div>' +
    '</div>';
  }
  container.innerHTML = html;
}

async function runJob(id, btn) {
  if (runningJobs.has(id)) return;
  runningJobs.add(id);
  btn.disabled = true;
  btn.className = 'btn-run running';
  btn.textContent = 'Running\u2026';
  try {
    const r = await fetch('/cron/run/' + id, {method: 'POST'});
    const d = await r.json();
    if (!d.ok) {
      btn.textContent = 'Failed';
      setTimeout(() => { btn.textContent = 'Run'; btn.className = 'btn-run'; btn.disabled = false; }, 3000);
    } else {
      btn.textContent = 'Triggered';
      // Refresh cron data after a short delay to pick up new state
      setTimeout(() => { loadCron(); }, 5000);
    }
  } catch(e) {
    btn.textContent = 'Error';
    setTimeout(() => { btn.textContent = 'Run'; btn.className = 'btn-run'; btn.disabled = false; }, 3000);
  }
  runningJobs.delete(id);
}

function fmtDuration(ms) {
  if (ms < 1000) return ms + 'ms';
  const s = ms / 1000;
  if (s < 60) return s.toFixed(1) + 's';
  const m = Math.floor(s / 60);
  const sec = Math.floor(s % 60);
  return m + 'm ' + sec + 's';
}

function esc(s) {
  if (!s) return '';
  const d = document.createElement('div');
  d.textContent = s;
  return d.innerHTML;
}

checkStatus();
setInterval(checkStatus, 10000);
loadCron();
setInterval(loadCron, 30000);
</script>
</body>
</html>`

// --- Main ---

func main() {
	token := os.Getenv("OPS_TOKEN")
	if token == "" {
		log.Fatal("OPS_TOKEN environment variable is required")
	}

	port := os.Getenv("OPS_PORT")
	if port == "" {
		port = "18790"
	}

	openclawBin := os.Getenv("OPENCLAW_BIN")
	if openclawBin == "" {
		openclawBin = "openclaw"
	}

	// Background health checker — every 30s, cache last 120 checks (1 hour)
	cache := newHealthCache(120)
	go func() {
		for {
			cache.add(checkGatewayHealth())
			time.Sleep(30 * time.Second)
		}
	}()

	startTime := time.Now()

	mux := http.NewServeMux()

	// UI — served at root, protected by Cloudflare Access
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, uiHTML)
	})

	// Health probe — no auth (used by watchdog and uptime monitors)
	// Includes cron summary so external dashboards can surface job health.
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		resp := healthResponse{
			OK:      true,
			Message: "ops-gateway healthy",
			Time:    time.Now().UTC().Format(time.RFC3339),
		}
		if data, err := readCronData(); err == nil {
			resp.Cron = &data.Summary
		}
		writeJSON(w, http.StatusOK, resp)
	})

	// Status — Cloudflare Access OR bearer token
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		if !hasCloudflareAccess(r) && !checkBearerToken(r, token) {
			writeJSON(w, http.StatusUnauthorized, response{
				OK: false, Message: "unauthorized",
				Time: time.Now().UTC().Format(time.RFC3339),
			})
			return
		}

		current, ok := cache.latest()
		if !ok {
			current = checkGatewayHealth()
		}

		uptime := time.Since(startTime).Round(time.Second)
		uptimeStr := formatDuration(uptime)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(statusResponse{
			Current: current,
			History: cache.all(),
			Uptime:  uptimeStr,
		})
	})

	// Restart — Cloudflare Access OR bearer token
	mux.HandleFunc("POST /restart", func(w http.ResponseWriter, r *http.Request) {
		if !hasCloudflareAccess(r) && !checkBearerToken(r, token) {
			writeJSON(w, http.StatusUnauthorized, response{
				OK: false, Message: "unauthorized",
				Time: time.Now().UTC().Format(time.RFC3339),
			})
			return
		}

		log.Println("restart requested")
		cmd := exec.Command("systemctl", "--user", "restart", "openclaw-gateway")
		output, err := cmd.CombinedOutput()
		if err != nil {
			msg := fmt.Sprintf("restart failed: %v: %s", err, string(output))
			log.Println(msg)
			writeJSON(w, http.StatusInternalServerError, response{
				OK: false, Message: msg,
				Time: time.Now().UTC().Format(time.RFC3339),
			})
			return
		}

		log.Println("restart succeeded")
		writeJSON(w, http.StatusOK, response{
			OK:      true,
			Message: "openclaw-gateway restarted",
			Time:    time.Now().UTC().Format(time.RFC3339),
		})
	})

	// Doctor — Cloudflare Access OR bearer token
	mux.HandleFunc("POST /doctor", func(w http.ResponseWriter, r *http.Request) {
		if !hasCloudflareAccess(r) && !checkBearerToken(r, token) {
			writeJSON(w, http.StatusUnauthorized, response{
				OK: false, Message: "unauthorized",
				Time: time.Now().UTC().Format(time.RFC3339),
			})
			return
		}

		fix := r.URL.Query().Get("fix") == "true"
		args := []string{"doctor"}
		if fix {
			args = append(args, "--fix")
		}

		log.Printf("doctor requested (fix=%v)", fix)
		cmd := exec.Command(openclawBin, args...)
		output, err := cmd.CombinedOutput()
		ok := err == nil

		log.Printf("doctor complete (ok=%v, output=%d bytes)", ok, len(output))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(doctorResponse{
			OK:     ok,
			Output: string(output),
			Time:   time.Now().UTC().Format(time.RFC3339),
		})
	})

	// Cron list — Cloudflare Access OR bearer token
	mux.HandleFunc("GET /cron", func(w http.ResponseWriter, r *http.Request) {
		if !hasCloudflareAccess(r) && !checkBearerToken(r, token) {
			writeJSON(w, http.StatusUnauthorized, response{
				OK: false, Message: "unauthorized",
				Time: time.Now().UTC().Format(time.RFC3339),
			})
			return
		}

		data, err := readCronData()
		if err != nil {
			log.Printf("cron list error: %v", err)
			writeJSON(w, http.StatusInternalServerError, response{
				OK: false, Message: err.Error(),
				Time: time.Now().UTC().Format(time.RFC3339),
			})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(data)
	})

	// Cron run — trigger a job manually
	mux.HandleFunc("POST /cron/run/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !hasCloudflareAccess(r) && !checkBearerToken(r, token) {
			writeJSON(w, http.StatusUnauthorized, response{
				OK: false, Message: "unauthorized",
				Time: time.Now().UTC().Format(time.RFC3339),
			})
			return
		}

		jobID := r.PathValue("id")
		if jobID == "" {
			writeJSON(w, http.StatusBadRequest, response{
				OK: false, Message: "missing job id",
				Time: time.Now().UTC().Format(time.RFC3339),
			})
			return
		}

		// Validate job exists
		data, err := readCronData()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, response{
				OK: false, Message: "failed to read cron data",
				Time: time.Now().UTC().Format(time.RFC3339),
			})
			return
		}
		found := false
		for _, j := range data.Jobs {
			if j.ID == jobID {
				found = true
				break
			}
		}
		if !found {
			writeJSON(w, http.StatusNotFound, response{
				OK: false, Message: "job not found",
				Time: time.Now().UTC().Format(time.RFC3339),
			})
			return
		}

		log.Printf("cron run requested: %s", jobID)
		cmd := exec.Command(openclawBin, "cron", "run", jobID)
		output, err := cmd.CombinedOutput()

		resp := cronRunResponse{
			OK:    err == nil,
			JobID: jobID,
			Time:  time.Now().UTC().Format(time.RFC3339),
		}
		if err != nil {
			resp.Message = fmt.Sprintf("run failed: %v", err)
			resp.Output = string(output)
			log.Printf("cron run failed: %s: %v", jobID, err)
		} else {
			resp.Message = "job triggered"
			resp.Output = string(output)
			log.Printf("cron run triggered: %s", jobID)
		}

		status := http.StatusOK
		if !resp.OK {
			status = http.StatusInternalServerError
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(resp)
	})

	addr := "127.0.0.1:" + port
	log.Printf("ops-gateway listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

// --- Helpers ---

func hasCloudflareAccess(r *http.Request) bool {
	return r.Header.Get("Cf-Access-Authenticated-User-Email") != ""
}

func checkBearerToken(r *http.Request, token string) bool {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	provided := strings.TrimPrefix(auth, "Bearer ")
	return subtle.ConstantTimeCompare([]byte(provided), []byte(token)) == 1
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
