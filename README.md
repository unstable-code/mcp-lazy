# mcp-lazy

`lazymcp` wraps a stdio [MCP](https://modelcontextprotocol.io) server so that it is
only started when a client actually uses it.

MCP clients such as Claude Code start every configured stdio server when a session
starts and keep it until the session ends, whether or not a single tool is called.
With many sessions open (a terminal multiplexer restoring a dozen of them at once is
enough), idle servers add up: one `npx chrome-devtools-mcp` costs three processes and
about 35 MB resident plus ~285 MB swapped, per session.

With `lazymcp` in front, a session that never calls a tool holds one ~5 MB process and
no server at all.

## How it works

```
client ──stdio── lazymcp ──stdio── server (started on first use)
```

1. **Before the server exists**, `lazymcp` answers the session-setup requests
   (`server/discover`, `initialize`, `tools/list`, and the other list methods) from
   answers it recorded from the same server in an earlier session. `ping` and
   `logging/setLevel` are answered directly.
2. **The first request it cannot answer** (normally the first `tools/call`) starts the
   server. `lazymcp` replays the client's own `initialize`, `notifications/initialized`
   and log level to it, swallows the server's answers (the client already has them),
   then forwards the waiting request.
3. **From then on** it relays bytes in both directions without parsing them. Requests
   the server sends to the client (such as `roots/list`) pass through untouched.

The only cost is latency on that first tool call: the time the server needs to start
(about 1.3 s for `chrome-devtools-mcp` with a warm npm cache).

### The cache

- One file per wrapped command line, under `--cache-dir`
  (default `$XDG_CACHE_HOME/lazymcp`). Changing the server version or its flags changes
  the command line, so it starts a fresh cache.
- Keys include the protocol version, because servers echo the version the client asked
  for in `initialize`.
- A cold cache costs nothing extra: the server is started right away, exactly as without
  `lazymcp`, and the answers are recorded on the way through.
- Error answers are not recorded, except `-32601 Method not found`, which is a fixed
  property of the server (it is how servers answer Claude Code's `server/discover`
  probe).
- When the server does start, `lazymcp` re-asks it any list the client got from the
  cache. If the answer changed, the cache is corrected and the client receives
  `notifications/tools/list_changed` (or the prompts/resources equivalent).

### Process cleanup

The server runs in its own process group. When the client hangs up, `lazymcp` closes
the server's stdin, waits up to 5 s, then sends `SIGTERM` (and `SIGKILL` after 2 s more)
to the group. On Linux the server also gets `SIGTERM` from the kernel if `lazymcp` itself
is killed (`PR_SET_PDEATHSIG`).

Processes that move themselves into another group are left to their own parent-death
handling, as they would be without `lazymcp`. For `chrome-devtools-mcp` that covers the
telemetry watchdog and the browser: all 16 descendants were gone within 8 s after both a
clean hang-up and `kill -9` of `lazymcp`.

## Usage

```
lazymcp [OPTION]... -- COMMAND [ARG]...

      --cache-dir DIR  keep recorded server answers in DIR; empty disables the cache
  -v, --verbose        log cache hits and recordings to stderr
  -h, --help           show this help and exit
      --version        print the version and exit
```

Claude Code (`.mcp.json`, or `claude mcp add`):

```json
{
  "mcpServers": {
    "chrome-devtools": {
      "type": "stdio",
      "command": "lazymcp",
      "args": ["--", "npx", "-y", "chrome-devtools-mcp@latest", "--no-usage-statistics"]
    }
  }
}
```

Tool names do not change: the client still sees the wrapped server's name and tools.

## Install

Prebuilt binaries for linux and darwin (amd64, arm64) are attached to each
[release](https://github.com/unstable-code/mcp-lazy/releases), with a `SHA256SUMS`
file:

```sh
curl -fLO https://github.com/unstable-code/mcp-lazy/releases/latest/download/lazymcp-linux-amd64
chmod +x lazymcp-linux-amd64 && mv lazymcp-linux-amd64 ~/.local/bin/lazymcp
```

Or build it:

```sh
nix run github:unstable-code/mcp-lazy -- --help
go install github.com/unstable-code/mcp-lazy/cmd/lazymcp@latest
```

As a flake input, use `packages.<system>.default`. The binary is static
(`CGO_ENABLED=0`) and uses the standard library only. Unix-like systems only; parent
death signalling is Linux-only.

## Development

```sh
nix develop          # go, gopls
go test -race ./...
```

The tests replay the opening sequence captured from Claude Code 2.1.287 against
`chrome-devtools-mcp` 1.3.0 (string and numeric ids, varying key order, a
`server/discover` probe answered with `-32601`, server-initiated `roots/list`), plus a
`tools/call` payload with quotes, escapes, Hangul and shell metacharacters that must
reach the server byte for byte.

## Limitations

- stdio servers only.
- A server whose list of tools depends on something other than its command line
  (environment, files) can be served a stale list until it first starts; it is then
  corrected via `list_changed`.
- Paginated list requests (with a `cursor`) start the server.

## License

[MIT](LICENSE)
