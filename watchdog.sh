#!/usr/bin/env bash
# openclaw-watchdog — checks gateway health, auto-restarts on failure,
# alerts via Telegram when OpenClaw channels are unavailable.
#
# Designed to run as a systemd timer (every 2 minutes).
# State file tracks consecutive failures to avoid alert spam.

set -euo pipefail

STATE_FILE="/tmp/openclaw-watchdog-state"
GATEWAY_URL="http://127.0.0.1:18789"
MAX_FAILURES_BEFORE_RESTART=3
MAX_FAILURES_BEFORE_ALERT=3

# Telegram config — direct API, bypasses OpenClaw entirely
TELEGRAM_TOKEN=$(cat ~/.openclaw/credentials/telegram-bot-token 2>/dev/null || echo "")
TELEGRAM_CHAT_ID="8702871653"

send_telegram() {
    local msg="$1"
    if [[ -z "$TELEGRAM_TOKEN" ]]; then
        echo "WARNING: No Telegram token, cannot alert: $msg"
        return
    fi
    curl -s -X POST "https://api.telegram.org/bot${TELEGRAM_TOKEN}/sendMessage" \
        -d chat_id="$TELEGRAM_CHAT_ID" \
        -d text="$msg" \
        -d parse_mode="Markdown" \
        > /dev/null 2>&1 || echo "WARNING: Telegram send failed"
}

# Read state
failures=0
last_alert=0
if [[ -f "$STATE_FILE" ]]; then
    failures=$(sed -n '1p' "$STATE_FILE" 2>/dev/null || echo 0)
    last_alert=$(sed -n '2p' "$STATE_FILE" 2>/dev/null || echo 0)
fi

# Health check — try the gateway HTTP endpoint
healthy=false
if curl -sf -o /dev/null -m 5 "$GATEWAY_URL" 2>/dev/null; then
    healthy=true
fi

# Also check if the systemd service is active
service_active=false
if systemctl --user is-active openclaw-gateway > /dev/null 2>&1; then
    service_active=true
fi

now=$(date +%s)

if $healthy && $service_active; then
    # Healthy — reset state
    if (( failures >= MAX_FAILURES_BEFORE_ALERT )); then
        send_telegram "✅ *OpenClaw recovered* — gateway is healthy again after $failures consecutive failures."
    fi
    echo "0" > "$STATE_FILE"
    echo "$last_alert" >> "$STATE_FILE"
    exit 0
fi

# Unhealthy
failures=$((failures + 1))
echo "$failures" > "$STATE_FILE"
echo "$last_alert" >> "$STATE_FILE"

echo "$(date -Iseconds) — gateway unhealthy (failure #$failures, service_active=$service_active, healthy=$healthy)"

# Auto-restart after threshold
if (( failures >= MAX_FAILURES_BEFORE_RESTART )); then
    echo "Attempting auto-restart..."
    if systemctl --user restart openclaw-gateway 2>&1; then
        # Wait a few seconds and recheck
        sleep 5
        if systemctl --user is-active openclaw-gateway > /dev/null 2>&1; then
            echo "Auto-restart succeeded"
            # Alert if this was the first restart attempt
            if (( failures == MAX_FAILURES_BEFORE_RESTART )); then
                send_telegram "🔄 *OpenClaw auto-restarted* — gateway was unhealthy for $failures checks. Restart succeeded."
            fi
        else
            echo "Auto-restart FAILED — service still not active"
            # Only alert once per incident (every 10 minutes = 5 checks)
            if (( now - last_alert > 600 )); then
                send_telegram "🔴 *OpenClaw DOWN* — auto-restart failed after $failures consecutive failures. Manual intervention needed.

\`systemctl --user restart openclaw-gateway\`
Or use: \`curl -X POST https://ops.forgedthought.ai/restart -H 'Authorization: Bearer <token>'\`"
                echo "$failures" > "$STATE_FILE"
                echo "$now" >> "$STATE_FILE"
            fi
        fi
    else
        echo "systemctl restart command failed"
        if (( now - last_alert > 600 )); then
            send_telegram "🔴 *OpenClaw DOWN* — systemctl restart failed. Manual intervention needed."
            echo "$failures" > "$STATE_FILE"
            echo "$now" >> "$STATE_FILE"
        fi
    fi
elif (( failures == MAX_FAILURES_BEFORE_ALERT )); then
    send_telegram "⚠️ *OpenClaw unhealthy* — $failures consecutive failures detected. Auto-restart will trigger on next check."
fi
