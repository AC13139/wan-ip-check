# =============================================================================
# Dockerfile for IP Address Monitor Container
# =============================================================================
# This Dockerfile builds a lightweight Alpine-based container to run the IP
# address monitoring script with cron scheduling and Discord notifications.

# Base Image: Use the official Alpine Linux image for minimal footprint.
FROM alpine:latest

# Working Directory: Set the application root directory.
WORKDIR /app

# Dependencies: Install required packages for script execution and scheduling.
# - bash: Shell interpreter for running scripts.
# - curl: Tool for HTTP requests (IP fetching and webhook notifications).
# - cronie: Cron daemon for task scheduling.
# Also create a log directory and clean up cache to optimize image size.
RUN apk update && \
    apk add --no-cache bash curl cronie && \
    mkdir -p /var/log/cron && \
    rm -rf /var/cache/apk/*

# Application Files: Copy the IP monitoring script and Docker entrypoint script.
COPY ip_check.sh /app/ip_check.sh
COPY entrypoint.sh /app/entrypoint.sh

# Permissions: Grant execute permissions to the scripts.
RUN chmod +x /app/ip_check.sh /app/entrypoint.sh

# Entrypoint: Define the container's startup command.
# The entrypoint script configures cron and maintains the container's lifecycle.
ENTRYPOINT ["/app/entrypoint.sh"]
