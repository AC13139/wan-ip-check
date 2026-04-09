#!/bin/bash

# =============================================================================
# IP Address Change Monitor Script (Enhanced with Daily Summary)
# =============================================================================
# This script monitors changes in the public IP address and sends notifications
# via Discord webhook. It supports binding to specific network interfaces for
# IP checking and notifications, and uses an HTTP proxy for Discord API access.
# It also sends a daily "No Change" summary after a specific time.

# Configuration Section
# ---------------------

# Discord webhook URL for sending notifications (required).
# IMPORTANT: Discord API is blocked in China, so a proxy is required.
WEBHOOK_URL="${DISCORD_WEBHOOK_URL}"

# HTTP Proxy for Discord Notifications (Required for access from China)
# This proxy is used ONLY for sending notifications to Discord.
DISCORD_PROXY="${DISCORD_PROXY:?ERROR: DISCORD_PROXY environment variable is not set. Exiting.}"

# Path to the file storing the last known public IP address.
IP_FILE="./app/last_public_ip.txt"

# --- [NEW CONFIGURATION] Daily Summary Settings ---
# Path to the file storing the date of the last "No Change" notification.
NO_CHANGE_TRACK_FILE="./app/last_daily_summary_date.txt"
# The time after which the daily summary should be sent (Format: HHMM, 24-hour clock).
# Example: 2000 for 8 PM, 0830 for 8:30 AM.
DAILY_SUMMARY_TIME="${DAILY_SUMMARY_TIME:-2000}"
# --------------------------------------------------

# Ensure the directory for the IP files exists.
# Uses dirname to extract the directory path and mkdir -p to create it if needed.
mkdir -p "$(dirname "$IP_FILE")"

# Service URL for querying the current public IP address.
# This service should be accessible directly without a proxy.
IP_SERVICE="${IP_CHECK_SERVICE:-https://icanhazip.com}"

# Network interface for IP checking operations.
# This interface must have direct internet access to check the public IP.
IP_CHECK_INTERFACE="${IP_CHECK_INTERFACE:-en1}"

# Network interface for sending notification requests.
# This interface must be able to reach the proxy server defined in DISCORD_PROXY.
NOTIFICATION_INTERFACE="${NOTIFICATION_INTERFACE:-en0}"

# End Configuration
# -----------------

# Validate required environment variables.
if [ -z "$WEBHOOK_URL" ]; then
    echo "ERROR: DISCORD_WEBHOOK_URL environment variable is not set. Exiting."
    exit 1
fi

# -----------------------------------------------------------------------------
# Function: send_discord_alert (For IP Changes)
# -----------------------------------------------------------------------------
# Sends a critical notification to Discord when an IP change is detected.
# Uses Proxy.
# -----------------------------------------------------------------------------
send_discord_alert() {
    local OLD_IP="$1"
    local NEW_IP="$2"

    # Define the embed color (blue: 0x3498DB in decimal).
    local COLOR=3447003

    # Construct the JSON payload for the Discord webhook.
    local JSON_PAYLOAD=$(cat <<EOF
{
  "username": "Public IP Alert",
  "avatar_url": "https://i.imgur.com/gK9wLgS.png",
  "embeds": [
    {
      "title": ":globe_with_meridians: Public IP Address Change Detected!",
      "description": "Your public IP address has been updated by the ISP.",
      "color": $COLOR,
      "fields": [
        {
          "name": "Old IP Address",
          "value": "$OLD_IP",
          "inline": true
        },
        {
          "name": "New IP Address",
          "value": "**$NEW_IP**",
          "inline": true
        }
      ],
      "timestamp": "$(date -u +'%Y-%m-%dT%H:%M:%S.000Z')",
      "footer": {
        "text": "IP Check Script - Critical Alert"
      }
    }
  ]
}
EOF
)

    # Send the notification using curl via PROXY.
    echo "$(date): Sending IP CHANGE notification to Discord via proxy ($DISCORD_PROXY)..."
    HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
         -H "Content-Type: application/json" \
         -X POST \
         -x "$DISCORD_PROXY" \
         -d "$JSON_PAYLOAD" \
         "$WEBHOOK_URL")

    if [ "$HTTP_CODE" -ge 200 ] && [ "$HTTP_CODE" -lt 300 ] 2>/dev/null; then
        echo "$(date): Notification sent successfully (HTTP $HTTP_CODE)."
    else
        echo "$(date): ERROR: Failed to send notification via proxy (HTTP $HTTP_CODE)."
    fi
}

# -----------------------------------------------------------------------------
# [NEW FUNCTION]: send_no_change_alert (Daily Summary)
# -----------------------------------------------------------------------------
# Sends a daily summary notification if no IP change occurred by a certain time.
# Uses Proxy.
# -----------------------------------------------------------------------------
send_no_change_alert() {
    local CURRENT_IP="$1"

    # Define the embed color (Green: 0x2ECC71 in decimal for "All Good").
    local COLOR=3066993

    # Construct JSON payload. Simpler than the alert one.
    local JSON_PAYLOAD=$(cat <<EOF
{
  "username": "Public IP Monitor",
  "avatar_url": "https://i.imgur.com/gK9wLgS.png",
  "embeds": [
    {
      "title": ":white_check_mark: Daily IP Summary: No Change",
      "description": "As of $(date +'%H:%M'), the public IP address remains unchanged today.",
      "color": $COLOR,
      "fields": [
        {
          "name": "Current IP Address",
          "value": "**$CURRENT_IP**",
          "inline": false
        }
      ],
      "timestamp": "$(date -u +'%Y-%m-%dT%H:%M:%S.000Z')",
      "footer": {
        "text": "IP Check Script - Daily Summary"
      }
    }
  ]
}
EOF
)

    # Send the notification using curl via PROXY.
    echo "$(date): Sending DAILY SUMMARY notification to Discord via proxy ($DISCORD_PROXY)..."
    HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
         -H "Content-Type: application/json" \
         -X POST \
         -x "$DISCORD_PROXY" \
         -d "$JSON_PAYLOAD" \
         "$WEBHOOK_URL")

    if [ "$HTTP_CODE" -ge 200 ] && [ "$HTTP_CODE" -lt 300 ] 2>/dev/null; then
        echo "$(date): Daily summary sent successfully (HTTP $HTTP_CODE)."
    else
        echo "$(date): ERROR: Failed to send daily summary via proxy (HTTP $HTTP_CODE)."
    fi
}


# Main Script Logic
# -----------------

# Step 1: Retrieve the current public IP address (Direct connection).
#NEW_IP=$(curl -s --interface $IP_CHECK_INTERFACE $IP_SERVICE)
NEW_IP=$(curl -s $IP_SERVICE)
echo "DEBUG: Current IP response from '$IP_SERVICE': '$NEW_IP'"

# Validate IP.
if [ -z "$NEW_IP" ] || [[ ! "$NEW_IP" =~ ^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$ ]]; then
    echo "$(date): ERROR: Failed to retrieve a valid IP address. Output: '$NEW_IP'"
    exit 1
fi

# Step 2: Read the last known IP.
LAST_IP=""
if [ -f "$IP_FILE" ]; then
    LAST_IP=$(cat "$IP_FILE")
fi

# Step 3: Compare and act.
if [ "$NEW_IP" != "$LAST_IP" ]; then
    # --- Case: IP Changed ---
    echo "$(date): IP Change Detected! Old: ${LAST_IP:-None}, New: $NEW_IP."

    if [ -z "$LAST_IP" ]; then
        # First run: just save.
        echo "$NEW_IP" > "$IP_FILE"
        echo "$(date): First run. IP saved, no alert sent."
    else
        # Actual change: send alert and update.
        send_discord_alert "$LAST_IP" "$NEW_IP"
        echo "$NEW_IP" > "$IP_FILE"
        echo "$(date): IP file updated."
    fi
    # Note: If IP changed, we don't send the "No Change" summary today.
    # We update the tracking file to today so it doesn't trigger later.
    date +%Y-%m-%d > "$NO_CHANGE_TRACK_FILE"

else
    # --- Case: IP Unchanged ---
    echo "$(date): IP unchanged."

    # [NEW LOGIC]: Check if we need to send the daily summary.

    # 1. Get current time in HHMM format (e.g., 2015 for 8:15 PM).
    CURRENT_HHMM=$(date +%H%M)

    # 2. Check if current time is past the configured daily summary time.
    if [ "$CURRENT_HHMM" -ge "$DAILY_SUMMARY_TIME" ]; then

        # 3. Get today's date (YYYY-MM-DD).
        TODAY_DATE=$(date +%Y-%m-%d)
        LAST_SENT_DATE=""

        # 4. Read the date the last summary was sent.
        if [ -f "$NO_CHANGE_TRACK_FILE" ]; then
            LAST_SENT_DATE=$(cat "$NO_CHANGE_TRACK_FILE")
        fi

        # 5. Compare dates: If today's date is different from the last sent date, send it.
        if [ "$TODAY_DATE" != "$LAST_SENT_DATE" ]; then
            echo "$(date): Time past $DAILY_SUMMARY_TIME and summary not sent today. Sending now."

            send_no_change_alert "$NEW_IP"

            # Important: Update the tracking file with today's date so it only runs once.
            echo "$TODAY_DATE" > "$NO_CHANGE_TRACK_FILE"
        else
            # Debug log (optional, can be removed to reduce noise)
            # echo "DEBUG: Daily summary already sent for $TODAY_DATE."
            : # Do nothing command
        fi
    fi
fi
