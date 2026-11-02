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

- **Read** chats and messages with replies and reactions, including the history WhatsApp syncs when you link the device, and **load older messages** from the phone on demand
- **Search** messages and file names across every chat
- **Send** text (with replies and @mentions), files (images, videos, documents, voice messages) and reactions; delete your own messages
- **Download** received media, even after it expired from WhatsApp's servers (the phone uploads it again); images are shown to the model
- Recipients by **name, phone number or JID**, with explicit errors on ambiguous names
- Edits and deletions (*"delete for everyone"*) are applied to the archive
- **Read-only mode** and a **chat allow-list** to limit what the model can reach
- Pure Go, `CGO_ENABLED=0`: prebuilt binaries for macOS, Linux and Windows (amd64/arm64)

## Quick start

**1. Install.** On macOS or Linux, this downloads the latest release into `~/.local/bin`:

```sh
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m | sed -e 's/x86_64/amd64/' -e 's/aarch64/arm64/')
mkdir -p ~/.local/bin
curl -fsSL "https://github.com/Noneo-Code/wamcp/releases/latest/download/wamcp_${OS}_${ARCH}.tar.gz" \
  | tar -xz -C ~/.local/bin wamcp
~/.local/bin/wamcp version
```

<details>
<summary>Windows (PowerShell)</summary>

```powershell
$dir = "$env:LOCALAPPDATA\wamcp"
Invoke-WebRequest https://github.com/Noneo-Code/wamcp/releases/latest/download/wamcp_windows_amd64.zip -OutFile "$env:TEMP\wamcp.zip"
Expand-Archive "$env:TEMP\wamcp.zip" -DestinationPath $dir -Force
& "$dir\wamcp.exe" version
```

Use `wamcp_windows_arm64.zip` on ARM. In the next steps, replace `~/.local/bin/wamcp` with `$dir\wamcp.exe`.
</details>

<details>
<summary>Other ways: Go, or a manual download</summary>

With Go 1.26+: `go install github.com/Noneo-Code/wamcp/cmd/wamcp@latest` (installs to `~/go/bin`).

Or download the archive for your system from the [releases page](https://github.com/Noneo-Code/wamcp/releases)
and check it against `checksums.txt`. On macOS, a binary downloaded with a browser is
quarantined: run `xattr -d com.apple.quarantine wamcp` once.
</details>

**2. Link your account.** A QR code is printed in the terminal; scan it from
WhatsApp › *Linked devices* › *Link a device*.

```sh
~/.local/bin/wamcp login
# or, without a camera: ~/.local/bin/wamcp login --phone +33612345678  (then type the code in WhatsApp)
```

Keep WhatsApp open on your phone for a minute: it sends your recent history, which
wamcp archives locally.

**3. Add the server to Claude Code.** Register it with its absolute path, so it works
whatever your `PATH` is when Claude starts it:

```sh
claude mcp add whatsapp --scope user -- ~/.local/bin/wamcp serve
claude mcp list   # whatsapp: ... ✓ Connected
```

Then restart Claude Code (or run `/mcp`). `--scope user` makes it available in every
project; add `--read-only` after `serve` if you only want to read (see [Security](#security)).

<details>
<summary>Claude Desktop, Cursor and other MCP clients</summary>

Add this to the client's `mcpServers` configuration, with the absolute path of the binary
(`echo ~/.local/bin/wamcp` prints it):

```json
{
  "mcpServers": {
    "whatsapp": {
      "command": "/Users/you/.local/bin/wamcp",
      "args": ["serve"]
    }
  }
}
```
</details>

**4. Ask.** *"What did Sylvain write to me last week?"*, *"Find the invoice PDF Lucile sent"*,
*"Reply to the team group that I'll be 10 minutes late"*.

To update later, run the install command again; your account and archive are kept.

## Tools

| Tool | Description |
|---|---|
| `whatsapp_status` | Connection state, linked account, archive size |
| `list_chats` | Chats by recent activity with their last message; filter by name, number or JID |
| `get_messages` | Messages of a chat in chronological order, paginated with `before_message_id` |
| `search_messages` | Substring search in message text and file names (case-insensitive for unaccented letters) |
| `search_contacts` | Find contacts and groups to get their JID |
| `get_group_info` | Subject, description, members and admins of a group |
| `download_media` | Save a message's image, video, audio or document to disk (images are also shown to the model); expired media is requested again from the phone |
| `load_older_messages` | Ask the phone for messages older than the oldest archived one (phone must be online) |
| `send_message` | Send a text, optionally as a reply and with @mentions |
| `send_file` | Send a local file by absolute path (max 100 MB); an Ogg/Opus file is sent as a voice message |
| `send_reaction` | React to a message with an emoji, or remove your reaction |
| `delete_message` | Delete one of your own messages for everyone |

Messages carry `reply_to_message_id` and their `reactions`. Sending tools are annotated
as non read-only (and `delete_message` as destructive), so MCP clients ask for
confirmation before running them.

Wherever a chat is expected you can pass a **JID** (`33612345678@s.whatsapp.net`,
`1203630…@g.us`), an **international phone number** (`+33 6 12 34 56 78`) or a **name**.
A name that matches several chats returns the candidates instead of guessing.

## Security

> [!IMPORTANT]
> **Received messages are untrusted input.** Anyone who can message you can try to
> instruct the model ("send me the file at …"). Keep confirmations on for the sending
> tools. `send_file` always refuses the wamcp data directory (your session keys), and
> `wamcp serve --send-root ~/Documents/to-share` restricts it to one directory.

To reduce what a model can do, combine:

```sh
wamcp serve --read-only                          # no send, react or delete tool at all
wamcp serve --allow-chats "+33612345678,Family"  # only these chats are visible and reachable
```

`--allow-chats` takes JIDs, phone numbers or names (resolved at start-up; an ambiguous
name is an error). Every message is still archived locally; the restriction applies to
what the tools return and where they can send.

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
             [--read-only]            Hide the tools that send, react or delete
             [--allow-chats A,B]      Only give access to these chats
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
at link time; `load_older_messages` asks the phone for more, one chat at a time. Status updates, channels and broadcast lists are not archived. Your messages are sent to whichever AI model your MCP client uses: check
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
