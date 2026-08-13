## Summary

<!-- What does this PR change, and why? One or two paragraphs. -->

Closes #<!-- issue number, if any -->

## Type of change

- [ ] New Action type
- [ ] Bug fix
- [ ] Enhancement / feature
- [ ] Docs / tests / chore

## New Action type checklist

<!-- If adding a new Action type, confirm the walkthrough in CONTRIBUTING.md -->

- [ ] Implements the `ActionExecutor` interface in `action_<type>.go`
- [ ] Registered in `main.go`
- [ ] Credential fields listed in `SecretFields()` (encrypted at rest, masked in UI/API/MCP)
- [ ] `ValidateConfig` rejects every invalid shape
- [ ] Tests cover success and failure paths (`-race` clean)
- [ ] Documented in README **Action Types** + **Project Structure**

## UI changes

<!-- If the Web UI is touched at all -->

- [ ] Dark mode, existing design tokens only (no invented colors/fonts)
- [ ] Data/IP/ID values in `JetBrains Mono`, statuses as badges, secrets masked
- [ ] HTMX patterns only — no bespoke JS

## Verification

- [ ] `go vet ./...` passes
- [ ] `go test -race ./...` passes
- [ ] Manual test described below

<!-- How did you verify this works? -->

## License & DCO

- [ ] I agree that my contribution is licensed under the project's [GNU GPL v3.0](../LICENSE)
- [ ] I have certified the origin of my contribution under the
      [Developer Certificate of Origin 1.1](https://developercertificate.org/) —
      every commit carries a `Signed-off-by` line (`git commit -s`)
