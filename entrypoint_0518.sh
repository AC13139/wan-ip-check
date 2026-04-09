#!/bin/bash
set -e

## =============================================================================
## Docker Entrypoint Script for IP Address Monitor
## =============================================================================
## This script configures the cron daemon to run the IP check script periodically,
## injecting necessary environment variables and ensuring the container remains active.

## Define the default cron schedule (every 5 minutes).
DEFAULT_CRON="*/5 * * * *"

if [ -n "$IP_CHECK_INTERFACE" ]; then echo "IP Check Interface: $IP_CHECK_INTERFACE"; fi
if [ -n "$NOTIFICATION_INTERFACE" ]; then echo "Notification Interface: $NOTIFICATION_INTERFACE"; fi
## Determine the cron schedule from the CRON_SCHEDULE environment variable, or use the default.
CRON_JOB="${CRON_SCHEDULE:-$DEFAULT_CRON}"
echo "Using cron schedule: $CRON_JOB"
ENV_INJECTION="DISCORD_WEBHOOK_URL='$DISCORD_WEBHOOK_URL'"
if [ -n "$IP_CHECK_SERVICE" ]; then
    ENV_INJECTION="$ENV_INJECTION IP_CHECK_SERVICE='$IP_CHECK_SERVICE'"
fi
if [ -n "$IP_CHECK_INTERFACE" ]; then
    ENV_INJECTION="$ENV_INJECTION IP_CHECK_INTERFACE='$IP_CHECK_INTERFACE'"
fi
if [ -n "$NOTIFICATION_INTERFACE" ]; then
    ENV_INJECTION="$ENV_INJECTION NOTIFICATION_INTERFACE='$NOTIFICATION_INTERFACE'"
fi
echo "Environment variables for cron: $ENV_INJECTION"
## Step 1: Configure the Crontab
## Write the cron job to the crontab file, including environment variables.
## Output redirection ensures logs are captured by Docker.
## Using 'printf' with a newline ensures proper crontab formatting.
printf "%s\n" "$ENV_INJECTION $CRON_JOB /app/ip_check.sh >> /app/cron.log 2>&1" > /etc/crontabs/root

## Step 2: Start the Cron Daemon
echo "Starting cron daemon with schedule: $CRON_JOB"
## Launch crond in the background for compatibility with Alpine's cronie package.
crond &

## Step 2.5: Configure Policy-Based Routing for eth1 (VPN interface)
## Set up routing table 10 with default route via VPN gateway for eth1 traffic
ip route add default via 192.168.10.1 dev eth1 table 10 || echo "Default route for eth1 table 10 already exists or failed to add"
## Add policy rule to use table 10 for traffic from eth1 IP
ip rule add from 192.168.10.208 lookup 10 || echo "Policy rule for eth1 already exists or failed to add"


## Step 3: Keep the Container Running
echo "Container running in foreground..."
## Use 'tail -f /dev/null' to maintain a foreground process, preventing container exit.
exec tail -f /dev/null
