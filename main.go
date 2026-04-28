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
	"strings"
	"sync"
	"time"
)

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

// healthCache stores the last N health check results.
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
    display: flex; justify-content: center; align-items: center;
    min-height: 100vh; padding: 1rem;
  }
  .card {
    background: #1a1f2e; border-radius: 12px; padding: 2rem;
    max-width: 420px; width: 100%; box-shadow: 0 4px 24px rgba(0,0,0,0.4);
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
</style>
</head>
<body>
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

checkStatus();
setInterval(checkStatus, 10000);
</script>
</body>
</html>`

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
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, response{
			OK:      true,
			Message: "ops-gateway healthy",
			Time:    time.Now().UTC().Format(time.RFC3339),
		})
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

	addr := "127.0.0.1:" + port
	log.Printf("ops-gateway listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
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

// hasCloudflareAccess checks for the Cf-Access-Authenticated-User-Email header
// set by Cloudflare Access after successful authentication.
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
