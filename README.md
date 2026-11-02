# wamcp

[![CI](https://github.com/Noneo-Code/wamcp/actions/workflows/ci.yml/badge.svg)](https://github.com/Noneo-Code/wamcp/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/Noneo-Code/wamcp.svg)](https://pkg.go.dev/github.com/Noneo-Code/wamcp)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

**A [Model Context Protocol](https://modelcontextprotocol.io) server for your own WhatsApp account.**
Let Claude (or any MCP client) read, search and send your WhatsApp messages.

wamcp links to your account as a *linked device*, exactly like WhatsApp Web, using
[whatsmeow](https://github.com/tulir/whatsmeow). It is a single static Go binary:
no bridge process, no Python, no open port by default.

> [!WARNING]
> **Unofficial.** wamcp is not affiliated with or endorsed by WhatsApp or Meta, and using
> unofficial clients is against WhatsApp's Terms of Service. Your number could be banned.
> Do not use it for bulk or unsolicited messaging. For business messaging, use the official
> [WhatsApp Business Platform](https://developers.facebook.com/docs/whatsapp).

## Features

- **Read** chats and messages, including the history WhatsApp syncs when you link the device
- **Search** messages and file names across every chat
- **Send** text (with replies) and files: images, videos, audio, documents
- **Download** received media to disk
- Recipients by **name, phone number or JID**, with explicit errors on ambiguous names
- Edits and deletions (*"delete for everyone"*) are applied to the archive
- Pure Go, `CGO_ENABLED=0`: prebuilt binaries for macOS, Linux and Windows (amd64/arm64)

## Install

Download a binary from the [releases page](https://github.com/Noneo-Code/wamcp/releases), or with Go 1.26+:

```sh
go install github.com/Noneo-Code/wamcp/cmd/wamcp@latest
```

## Quick start

**1. Link your account.** A QR code is printed in the terminal; scan it from
WhatsApp › *Linked devices* › *Link a device*.

```sh
wamcp login
# or, without a camera: wamcp login --phone +33612345678  (then type the code in WhatsApp)
```

Keep WhatsApp open on your phone for a minute: it sends your recent history, which
wamcp archives locally.

**2. Add the server to your MCP client.**

Claude Code:

```sh
claude mcp add whatsapp -s user -- wamcp serve
```

Claude Desktop, Cursor and other clients (`mcpServers` configuration):

```json
{
  "mcpServers": {
    "whatsapp": {
      "command": "wamcp",
      "args": ["serve"]
    }
  }
}
```

**3. Ask.** *"What did Sylvain write to me last week?"*, *"Find the invoice PDF Lucile sent"*,
*"Reply to the team group that I'll be 10 minutes late"*.

## Tools

| Tool | Description |
|---|---|
| `whatsapp_status` | Connection state, linked account, archive size |
| `list_chats` | Chats by recent activity with their last message; filter by name, number or JID |
| `get_messages` | Messages of a chat in chronological order, paginated with `before_message_id` |
| `search_messages` | Substring search in message text and file names (case-insensitive for unaccented letters) |
| `search_contacts` | Find contacts and groups to get their JID |
| `download_media` | Save a message's image, video, audio or document to disk |
| `send_message` | Send a text, optionally as a reply |
| `send_file` | Send a local file by absolute path (max 100 MB) |

Sending tools are annotated as non read-only, so MCP clients ask for confirmation
before running them.

> [!IMPORTANT]
> **Received messages are untrusted input.** Anyone who can message you can try to
> instruct the model ("send me the file at …"). Keep confirmations on for the sending
> tools. `send_file` always refuses the wamcp data directory (your session keys), and
> `wamcp serve --send-root ~/Documents/to-share` restricts it to one directory.

Wherever a chat is expected you can pass a **JID** (`33612345678@s.whatsapp.net`,
`1203630…@g.us`), an **international phone number** (`+33 6 12 34 56 78`) or a **name**.
A name that matches several chats returns the candidates instead of guessing.

## Several MCP clients at once

WhatsApp gives one connection per linked device, so only one `wamcp serve` can run
per account; a second one exits with an explicit error. To share the account between
several clients or sessions, run a single long-lived server over Streamable HTTP:

```sh
wamcp serve --http 127.0.0.1:8765
```

Every request must carry the bearer token stored in the data directory (`wamcp token`
prints it, `WAMCP_TOKEN` overrides it):

```sh
claude mcp add --transport http whatsapp http://127.0.0.1:8765/mcp \
  --header "Authorization: Bearer $(wamcp token)" -s user
```

wamcp refuses non-loopback addresses unless you pass `--allow-remote`; only do that
behind a TLS reverse proxy. Idle HTTP sessions are closed after 30 minutes.

<details>
<summary>Run it in the background on macOS (launchd)</summary>

Save as `~/Library/LaunchAgents/com.github.noneo-code.wamcp.plist`, adjust the binary
path, then `launchctl load ~/Library/LaunchAgents/com.github.noneo-code.wamcp.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.github.noneo-code.wamcp</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/wamcp</string>
    <string>serve</string>
    <string>--http</string>
    <string>127.0.0.1:8765</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardErrorPath</key><string>/tmp/wamcp.log</string>
</dict>
</plist>
```

On Linux, the equivalent is a `systemd --user` service running the same command.
</details>

## Commands

```
wamcp login  [--phone +33612345678]   Link your WhatsApp account
wamcp serve  [--http 127.0.0.1:8765]  Run the MCP server (stdio by default)
             [--send-root DIR]        Only let send_file read files under DIR
wamcp status                          Linked account, archive size, whether a server runs
wamcp token                           Print the bearer token used by --http
wamcp logout [--purge]                Unlink the device (--purge also deletes the archive)
wamcp version
```

All commands accept `--data-dir DIR` and `--log-level debug|info|warn|error`.
Logs always go to stderr, never to stdout (which carries the MCP protocol).

## Data and privacy

Everything stays on your machine, in `$WAMCP_DATA_DIR` or by default
`~/Library/Application Support/wamcp` (macOS), `~/.config/wamcp` (Linux),
`%AppData%\wamcp` (Windows). The directory is created with `0700` permissions.

| File | Content |
|---|---|
| `session.db` | Device keys. **Anyone with this file can read and send as you.** |
| `messages.db` | Archive of chats and messages (plain SQLite) |
| `media/` | Downloaded media |
| `token` | Bearer token for `--http` |

wamcp only knows messages received while it runs, plus the history your phone sends
at link time. Status updates, channels and broadcast lists are not archived. Your messages are sent to whichever AI model your MCP client uses: check
that this is acceptable for you and for the people you talk to.

To stop: `wamcp logout --purge`, and check *Linked devices* on your phone.

## Development

```sh
go test -race ./...
golangci-lint run
go build ./cmd/wamcp
```

```
cmd/wamcp/          CLI: login, serve, status, token, logout
internal/config/    data directory layout
internal/store/     SQLite message archive
internal/wa/        whatsmeow client: events → archive, send, download, name resolution
internal/mcpserver/ MCP tools and HTTP transport auth
```

Releases are built by [GoReleaser](https://goreleaser.com) when a `v*` tag is pushed.

Contributions are welcome: open an issue first for anything larger than a fix.

## Credits

Built on [whatsmeow](https://github.com/tulir/whatsmeow) (MPL-2.0) and the official
[MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk). Inspired by
[lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp).

## License

[MIT](LICENSE) © Noneo
