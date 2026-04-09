#!/bin/bash
set -e

# =============================================================================
# Docker Entrypoint Script for IP Address Monitor
# FIX: Implemented a robust wrapper script to ensure cron execution reliability.
# FIX: Implemented idempotent (safe to re-run) Policy-Based Routing setup.
# =============================================================================

DEFAULT_CRON="*/5 * * * *"

# --- Configuration & Variable Setup ---

if [ -n "$IP_CHECK_INTERFACE" ]; then echo "IP Check Interface: $IP_CHECK_INTERFACE"; fi
if [ -n "$NOTIFICATION_INTERFACE" ]; then echo "Notification Interface: $NOTIFICATION_INTERFACE"; fi

# Determine the cron schedule
CRON_JOB="${CRON_SCHEDULE:-$DEFAULT_CRON}"
echo "Using cron schedule: $CRON_JOB"

# 1. Collect all required environment variables for the wrapper script.
# Helper: safely escape a value for single-quoted shell assignment.
escape_sq() { printf '%s' "$1" | sed "s/'/'\\\\''/g"; }

ENV_EXPORTS=""
add_env() {
    local name="$1" value="$2"
    if [ -n "$value" ]; then
        ENV_EXPORTS+="export ${name}='$(escape_sq "$value")'\n"
    fi
}

add_env DISCORD_WEBHOOK_URL "$DISCORD_WEBHOOK_URL"
add_env IP_CHECK_SERVICE    "$IP_CHECK_SERVICE"
add_env IP_CHECK_INTERFACE  "$IP_CHECK_INTERFACE"
add_env NOTIFICATION_INTERFACE "$NOTIFICATION_INTERFACE"
add_env DISCORD_PROXY       "$DISCORD_PROXY"
add_env DAILY_SUMMARY_TIME  "$DAILY_SUMMARY_TIME"
add_env PBR_GATEWAY         "$PBR_GATEWAY"
add_env PBR_DEV             "$PBR_DEV"
add_env PBR_TABLE           "$PBR_TABLE"

echo "Environment variables prepared for cron wrapper."


# --- CRON JOB SETUP (The Fix) ---

# 2. Generate the temporary wrapper script. This script sets the environment
# and then executes ip_check.sh, redirecting output to the container logs.
WRAPPER_SCRIPT="/tmp/cron_wrapper.sh"
printf "#!/bin/bash\nset -e\n\n" > "$WRAPPER_SCRIPT"
# Use printf %b to process the newline characters correctly
printf "%b" "$ENV_EXPORTS" >> "$WRAPPER_SCRIPT"
# Execute the main script, redirecting output to Docker logs via /proc/1/fd/X
printf "/app/ip_check.sh > /proc/1/fd/1 2>/proc/1/fd/2\n" >> "$WRAPPER_SCRIPT"

# Make the wrapper executable (owner-only to protect secrets)
chmod 700 "$WRAPPER_SCRIPT"
echo "Generated executable cron wrapper script: $WRAPPER_SCRIPT"

# 3. Configure the Crontab: Point cron directly to the executable wrapper script.
CRON_ENTRY="$CRON_JOB $WRAPPER_SCRIPT"
printf "%s\n" "$CRON_ENTRY" > /etc/crontabs/root
echo "Crontab entry written: $CRON_ENTRY"


# Remove stale PID file to prevent 'can't lock' errors on restart.
rm -f /var/run/crond.pid


# --- Policy-Based Routing (PBR) Setup (Idempotent Version) ---

# We only run this logic if NOTIFICATION_INTERFACE is set, as it typically handles
# the secondary interface setup (like a VPN).

if [ -n "$NOTIFICATION_INTERFACE" ]; then
    echo "Configuring PBR for Notification Interface: $NOTIFICATION_INTERFACE"

    # Attempt to extract the IP address of the specified interface
    CONTAINER_IP=$(ip addr show dev "$NOTIFICATION_INTERFACE" | grep 'inet ' | awk '{print $2}' | cut -d/ -f1)

    if [ -n "$CONTAINER_IP" ]; then
        # PBR configuration from environment variables (with defaults)
        PBR_GW="${PBR_GATEWAY:-192.168.10.1}"
        PBR_DEVICE="${PBR_DEV:-eth1}"
        PBR_TBL="${PBR_TABLE:-10}"

        ## Set up routing table with default route via gateway for notification traffic
        echo "Setting up routing table $PBR_TBL for $NOTIFICATION_INTERFACE of $CONTAINER_IP"
        ip route add default via "$PBR_GW" dev "$PBR_DEVICE" table "$PBR_TBL" || echo "Default route for $PBR_DEVICE table $PBR_TBL already exists or failed to add"
        ## Add policy rule to use routing table for traffic from notification interface IP
        echo "Adding policy rule for $NOTIFICATION_INTERFACE of $CONTAINER_IP"
        ip rule add from "$CONTAINER_IP" lookup "$PBR_TBL" || echo "Policy rule for $PBR_DEVICE already exists or failed to add"
    else
        echo "WARNING: Could not find IP address for interface $NOTIFICATION_INTERFACE. PBR setup skipped."
    fi
fi


# 4. Start the Cron Daemon as PID 1 (foreground)
# Using exec + crond -f ensures Docker can monitor the process and restart on failure.
echo "Starting cron daemon in foreground..."
exec crond -f
