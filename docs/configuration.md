# Configuration and operations

How the connection is scoped, how to turn the integration off for a
workspace, what the unsigned badge means, and how to drive all of it over
HTTP instead of the UI.

# Connection scope

**One connection serves every workspace.** The instance URL and token live in the
manifest's `config_schema`, which Kandev stores as a single plugin-level record
and renders at **Settings → Plugins → Forgejo**. Configure it once and it applies
everywhere.

The panel on the workspace integrations screen is a *status* surface, not a
second place to configure credentials: it reports whether the shared connection
is reachable and publishes that to the per-workspace enabled badge. It says so
on screen, because a per-workspace panel backed by a global credential is
otherwise easy to misread as a per-workspace setting.

If you need different Forgejo instances or different tokens per workspace, this
plugin does not support that today. It would mean moving the connection out of
`config_schema` and into workspace-scoped Host state and secrets, with explicit
save and disconnect actions — a deliberate change, not a configuration option.

# Enabling and disabling per workspace

The integrations card carries its own **enable** switch. Kandev does not render
one for a plugin integration — native integrations get theirs from the host, and
a plugin that wants parity has to supply it and publish the result with
`host.setIntegrationEnabled`. That is why this card had no toggle before 0.1.2.

The switch is not decorative. The choice is stored per workspace on the backend,
which honors it: while an integration is off, repository, branch, review and
association reads return nothing, so the native pickers render empty instead of
showing a provider the operator turned off, and pull-request create, link and
unlink refuse outright rather than silently doing nothing. Turning it back on
restores everything. A workspace with no stored choice is enabled — installing
the plugin is the opt-in.

Disabling is not uninstalling: config, credentials and stored task links all
survive, and the plugin stays installed for other workspaces.

# Headless install and configuration

The UI flow above is the normal path. Everything it does is reachable over HTTP,
which is what scripted and headless installs need. All routes are Kandev's, not
this plugin's.

```sh
KANDEV=https://kandev.example.com
ID=kandev-plugin-forgejo
VERSION=0.1.2

# Install from a local package...
curl -sf -X POST "$KANDEV/api/plugins/install" -F "package=@$ID-$VERSION.tar.gz"
# ...or straight from a release URL.
curl -sf -X POST "$KANDEV/api/plugins/install" \
  -H 'Content-Type: application/json' \
  -d "{\"url\":\"https://github.com/naerymdan/$ID/releases/download/v$VERSION/$ID-$VERSION.tar.gz\"}"

# Configure. The `config` wrapper is required: a flat body is rejected with a
# misleading `missing required field "base_url"`.
curl -sf -X PATCH "$KANDEV/api/plugins/$ID" \
  -H 'Content-Type: application/json' \
  -d '{"config":{"base_url":"https://forgejo.example.com","api_token":"<token>"}}'

# Invoke an action. The `body` wrapper is required too: omitting it returns
# 503 `plugin action unavailable`, with the real cause (`unexpected end of
# JSON input`) only in the server log.
curl -sf -X POST "$KANDEV/api/plugins/$ID/actions/connection.test" \
  -H 'Content-Type: application/json' \
  -d '{"workspaceId":"<workspace-id>"}'
```

Every action in this plugin is `scope: "workspace"`, so **`workspaceId` is
required on all of them**. Kandev validates the envelope before dispatching, so
a missing selector fails with a 400 that never reaches the plugin process.

Action bodies:

| Action | Body |
| --- | --- |
| `connection.get` / `connection.test` | none |
| `connection.set_enabled` | `{"enabled":true}` |
| `repositories.list` | `{"query":"","cursor":"","limit":100}` |
| `repositories.inspect` | `{"url":"https://forgejo.example.com/owner/repo"}` |
| `repositories.branches` | `{"repository":{…full descriptor…}}` — a flat identity is rejected |
| `change_requests.get` / `.associations` | none |
| `change_requests.create` | `{"title","description","destination","draft"}` (also needs `taskId`, `sessionId`, `repositoryId`) |
| `change_requests.link` | `{"reference":"owner/repo#1"}` (also needs `taskId`) |
| `change_requests.unlink` | `{"connection_scope","repository_id","number"}` (also needs `taskId`) |

> **`DELETE /api/plugins/{id}` uninstalls immediately.** There is no
> confirmation step and it removes the plugin's config, state, and secrets. It
> is easy to hit while probing routes. This is Kandev's API, not this plugin's,
> but it is worth knowing before you script against it.

# The "unsigned" badge

Every plugin shows as unsigned at **Settings → Plugins**, first-party ones
included. It is not a property of this package.

Kandev marks an install signed only when the package carries a
`checksums.txt.sig` **and** the host has a signature verifier wired
(`pkgtar.VerifySignature`). That hook is nil in the shipped product — it is
assigned only in the host's own tests — so the check short-circuits before any
signature is examined and every install is reported unsigned. The host is
deliberately honest here: it declines to claim a guarantee nothing verified.

This package therefore ships no `checksums.txt.sig`, and that is deliberate.
Adding one would not change the badge today, and it would add risk: a present
signature that fails verification fails the install outright, so a package
signed against one key would stop installing the day a host wires a verifier
expecting another. The archive's internal `checksums.txt` is still generated and
enforced on install, which detects corruption; it does not prove provenance.
Release provenance comes from the signed git tag and the GitHub release.

---

[← Back to the README](../README.md)
