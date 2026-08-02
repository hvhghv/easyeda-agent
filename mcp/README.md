# easyeda-agent MCP

Local stdio MCP adapter over the existing `easyeda` CLI/daemon. It exposes 11
tools: connection health, action discovery, one tool for each of the seven safe
action domains, circuit blocks, and the guarded workflow state machine. The
arbitrary-JavaScript debug domain is deliberately not exposed.

```bash
npm ci --ignore-scripts
EASYEDA_BIN=/absolute/path/to/easyeda npm test
EASYEDA_BIN=/absolute/path/to/easyeda npm start
```

Codex registration:

```bash
codex mcp add easyeda-agent \
  --env EASYEDA_BIN=/absolute/path/to/easyeda \
  -- node /absolute/path/to/mcp/src/server.mjs
```

The MCP process does not access EasyEDA directly. Mutations still pass through
the Go daemon, connector, workflow gates, audit log, and official `eda.*` API.
Mutating typed actions require both `project` and `doc`; use `easyeda_actions`
before calling a domain tool to inspect its typed payload. Workflow operations
use structured MCP fields instead of accepting arbitrary CLI options.

After registration, restart the MCP client so it discovers the new server. Run
`easyeda_health` first, then use `easyeda_actions` to select the exact typed
action. The Skill's inspect-before-mutate, save, reload, DRC, and workflow-gate
rules continue to apply to MCP calls.
