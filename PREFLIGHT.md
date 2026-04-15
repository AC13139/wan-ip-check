# Pre-Flight Checklist (Self-Review Before Codex)

Rules extracted from Phases 1-3 mistakes. Check EVERY item before submitting for review.

## Concurrency
- [ ] Any shared state accessed by both cron scheduler AND API handlers?
  → Needs per-resource mutex
- [ ] Goroutines launched outside the cron wrapper?
  → They won't be tracked by Stop(). Run synchronously or track with WaitGroup.
- [ ] Does creating/updating/deleting a resource require reloading the scheduler?
  → If yes, call Reload() from the handler.

## Database
- [ ] Multi-row write without transaction?
  → Wrap in Begin/Commit/Rollback.
- [ ] Foreign keys relied on?
  → Verify `PRAGMA foreign_keys = ON` is set.
- [ ] Append-only table (logs, history)?
  → Add retention/pruning.
- [ ] Time stored in DB?
  → Always UTC. Parse with explicit format.

## Secrets
- [ ] Any endpoint returning action config?
  → Mask secret fields.
- [ ] Masked value (•••••) could be round-tripped back to the server?
  → Merge with existing encrypted value, don't re-encrypt the mask.
- [ ] Encryption can fail?
  → Fail-closed. Never store plaintext on encryption error.
- [ ] Key file error?
  → Distinguish not-found (generate) from permission/IO error (fail).

## API Handlers
- [ ] Body size limited?
  → io.LimitReader on all POST/PUT.
- [ ] Input validated before writing to DB?
  → Cron expressions, timezones, HHMM ranges, URL schemes.
- [ ] Boolean field in JSON?
  → Zero-value is false. Document PUT semantics.
- [ ] Multiple settings updated in one request?
  → Validate ALL before writing ANY.
- [ ] Test/preview endpoint?
  → Must not mutate production state. Validate-only for Cloudflare.

## HTTP Clients
- [ ] New http.Client/Transport created per request?
  → Cache and reuse. Include all config dimensions in cache key.
- [ ] User-configured URL used in HTTP request?
  → Note SSRF risk. At minimum validate scheme is http/https.

## Error Handling
- [ ] DB write error ignored?
  → At minimum log it.
- [ ] Error message could leak secrets?
  → Never include config values in error responses.
- [ ] Partial success possible?
  → Document behavior. Don't mix success response with partial failure.
