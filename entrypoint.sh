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

# 1. Collect all required environment variables as 'export' statements.
# This variable will contain the contents of the wrapper script.
ENV_EXPORTS=""

if [ -n "$DISCORD_WEBHOOK_URL" ]; then
    ENV_EXPORTS+="export DISCORD_WEBHOOK_URL='$DISCORD_WEBHOOK_URL'\n"
fi
if [ -n "$IP_CHECK_SERVICE" ]; then
    ENV_EXPORTS+="export IP_CHECK_SERVICE='$IP_CHECK_SERVICE'\n"
fi
if [ -n "$IP_CHECK_INTERFACE" ]; then
    ENV_EXPORTS+="export IP_CHECK_INTERFACE='$IP_CHECK_INTERFACE'\n"
fi
if [ -n "$NOTIFICATION_INTERFACE" ]; then
    ENV_EXPORTS+="export NOTIFICATION_INTERFACE='$NOTIFICATION_INTERFACE'\n"
fi

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

# Make the wrapper executable
chmod +x "$WRAPPER_SCRIPT"
echo "Generated executable cron wrapper script: $WRAPPER_SCRIPT"

# 3. Configure the Crontab: Point cron directly to the executable wrapper script.
CRON_ENTRY="$CRON_JOB $WRAPPER_SCRIPT"
printf "%s\n" "$CRON_ENTRY" > /etc/crontabs/root
echo "Crontab entry written: $CRON_ENTRY"


# 4. Start the Cron Daemon
echo "Starting cron daemon..."
# Remove stale PID file to prevent 'can't lock' errors on restart.
rm -f /var/run/crond.pid
crond &


# --- Policy-Based Routing (PBR) Setup (Idempotent Version) ---

# We only run this logic if NOTIFICATION_INTERFACE is set, as it typically handles
# the secondary interface setup (like a VPN).

if [ -n "$NOTIFICATION_INTERFACE" ]; then
    echo "Configuring PBR for Notification Interface: $NOTIFICATION_INTERFACE"

    # Attempt to extract the IP address of the specified interface
    CONTAINER_IP=$(ip addr show dev "$NOTIFICATION_INTERFACE" | grep 'inet ' | awk '{print $2}' | cut -d/ -f1)

    if [ -n "$CONTAINER_IP" ]; then
        ## Set up routing table 10 with default route via VPN gateway for eth1 traffic
        echo "Setting up routing table 10 for $NOTIFICATION_INTERFACE of $CONTAINER_IP"
        ip route add default via 192.168.10.1 dev eth1 table 10 || echo "Default route for eth1 table 10 already exists or failed to add"
        ## Add policy rule to use table 10 for traffic from eth1 IP
        echo "Adding policy rule for $NOTIFICATION_INTERFACE of $CONTAINER_IP"
        ip rule add from "$CONTAINER_IP" lookup 10 || echo "Policy rule for eth1 already exists or failed to add"
    else
        echo "WARNING: Could not find IP address for interface $NOTIFICATION_INTERFACE. PBR setup skipped."
    fi
fi


# 5. Keep the Container Running
echo "Container running in foreground..."
# Use 'exec' to replace the current shell, making this the main process.
exec tail -f /dev/null
