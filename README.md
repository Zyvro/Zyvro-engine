# Zyvro Engine

The graph engine behind [Zyvro](https://zyv.ro), as a library.

A workflow is a directed graph of nodes. This module parses it, orders it
topologically, and runs each node against whichever provider that node names,
using the caller's own credentials. It is the same engine the hosted service and
[Zyvro Studio](https://github.com/Zyvro/Zyvro-desktop) both run, which is what
makes a workflow behave the same on a server and on a laptop.

```
go get github.com/Zyvro/Zyvro-engine
```

## Packages

| Package | What it is |
|---|---|
| `engine` | Graph parsing, DAG execution, the built-in nodes, the Brain agent, the replay cache |
| `providers` | Ollama, Google Gemini, Anthropic and OpenAI adapters, plus two subprocess providers |
| `plugins` | A sandboxed Lua runtime for user-authored nodes |
| `localstore` | A filesystem-backed workflow store, for running against a project folder |
| `mcp` | The JSON-RPC surface and tool catalogue both Zyvro MCP servers serve |
| `cmd/zyvrod` | The local daemon: one project folder, no accounts, no database |
| `cmd/zyvrel` | Signs engine releases |

## Running a workflow on a subscription, not an API key

Most people who pay for AI have a ChatGPT or Claude subscription rather than API
credit, and a hosted server cannot use one. Codex makes this concrete: in
subscription mode it authenticates against `wss://chatgpt.com/backend-api`, not
`api.openai.com`, and its access token is short-lived with a refresh token beside
it.

What does work is running the CLI the user already signed in to, on the machine
they signed in on. `providers` exposes `claude-cli` and `codex-cli` as ordinary
text providers that do exactly that. No credential reaches Zyvro; the engine
reads the CLI's stdout. They only resolve where the engine runs on the user's own
machine.

## Lua nodes

Node types are not a fixed list. A pack is a directory of `.lua` files and a
manifest, and its nodes appear in the palette and resolve by name like any
built-in.

```lua
return {
  type = "summarize",
  label = "Summarize",
  category = "AI",
  inputs = { "text" },
  outputs = { "text" },
  config = {
    { key = "sentences", label = "Sentences", type = "number", default = 3 },
  },
  run = function(ctx)
    return { text = ctx.llm{ prompt = "Summarize in " .. ctx.config.sentences .. " sentences:\n" .. ctx.input.text } }
  end,
}
```

Packs are meant to be installable from a store, which means a stranger's code
runs on someone's machine. A publish-time gate is a self-attestation by the party
you would be defending against, and review is reactive, so the sandbox is the
only real control. The blast radius is deliberately bounded to wasted CPU,
wasted model quota, and wrong output:

- Only `base`, `string`, `table` and `math` are opened. Never `io`, `os`,
  `package`, `debug`, `coroutine` or `channel`.
- Everything not on an explicit allow-list is deleted from the globals
  afterwards, and a test fails on any name a future Lua runtime adds rather than
  letting it through.
- Precompiled bytecode is refused at the door.
- A deadline stops a runaway script, and `pcall` cannot swallow it.
- A single Go builtin runs between two deadline checks, so the amplifiers are
  capped individually: `string.rep`, `string.format` widths, and the pattern
  functions whose replace step is quadratic.
- The registry bounds the Lua stack, not the Go heap, so a sampling watchdog
  bounds the heap separately and forces a collection before concluding.
- Model calls are counted against a per-run budget.
- File access exists on the node's context only when the pack declared the
  capability, and routes through the same gate the built-in file nodes use:
  symlinks resolved, nothing outside the project root, nothing inside `.zyvro/`.

## The local daemon

```sh
go build -o bin/zyvrod ./cmd/zyvrod
./bin/zyvrod --project /path/to/a/folder --port 0
```

It binds to `127.0.0.1` on a free port and prints one line of JSON carrying that
port and a bearer token every `/api/` call must present. Workflows live as files
under `.zyvro/` in the project folder, so they version alongside the code they
act on.

## Tests

```sh
go test ./...
```

No test calls a real model or a real CLI. Provider tests drive an `httptest`
server, the CLI providers point at a shell script written into `t.TempDir()`, and
the Lua host is given a fake. The suite costs nothing to run.

## Related

- [Zyvro-desktop](https://github.com/Zyvro/Zyvro-desktop) — Zyvro Studio
- [Zyvro-frontend](https://github.com/Zyvro/Zyvro-frontend) — the web app
