# reset-first

A CLIProxyAPI v8.0.69 scheduler plugin that spends the subscription quota that resets soonest first. It supports Claude and Codex subscriptions.

- Learns each subscription's 5-hour and weekly usage and resets from upstream headers: `anthropic-ratelimit-unified-5h-*` and `anthropic-ratelimit-unified-7d-*` for Claude, and the `x-codex-primary-*` and `x-codex-secondary-*` windows for Codex.
- Sends every request to the available subscription whose weekly quota resets soonest. Subscriptions whose reset is still unknown are tried first, so their next response reveals it.
- Moves a subscription that uses up its 5-hour or weekly limit to the end of the order until that limit resets, then sends requests to it again.
- Shows Claude's weekly Fable 5 usage from `anthropic-ratelimit-unified-7d_oi-*` on the status page. That limit applies only to Fable requests, so it does not change the order.
- The choice is deterministic, so a conversation stays on one account until that account becomes unavailable or reaches a limit, a subscription that resets sooner becomes available, or its quota window ends.
- Ranks API keys after subscriptions, and leaves pools without subscriptions, pools that mix providers, and all other providers to CPA.
- Makes no network calls, uses no host callbacks, and never reads OAuth tokens.

## Install

Add this repository's registry to CPA and install `reset-first` from the plugin store in the management dashboard:

```yaml
plugins:
  enabled: true
  dir: plugins
  store-sources:
    - "https://raw.githubusercontent.com/hosmelq/cliproxyapi-reset-first/main/registry.json"
  configs:
    reset-first:
      enabled: true
```

Or download `reset-first_<version>_<os>_<arch>.zip` for your platform from [Releases](https://github.com/hosmelq/cliproxyapi-reset-first/releases) and extract the library inside it into CPA's plugins directory. Releases cover Linux, macOS, and Windows on amd64 and arm64. Like CPA's own Linux release, the Linux builds need glibc 2.17 or newer. To build it yourself, name the library `reset-first.so` on Linux, `reset-first.dylib` on macOS, or `reset-first.dll` on Windows:

```sh
CGO_ENABLED=1 go build -buildmode=c-shared -o plugins/reset-first.so .
```

## Status page

Open **Reset First** from the plugin menu in the management dashboard to see, for each provider, which account requests go to, the order after it, and which accounts are not used right now and why. Each account shows how much of its 5-hour and weekly quota is left and when it resets, in the same format as CPA's quota page. Claude accounts also show their weekly Fable 5 limit once a response reports it. Usage comes from each account's latest response through CPA. The page reads `GET /v0/management/reset-first/status`, which requires the management key. It reuses the dashboard login when "Remember password" is on, and otherwise asks for the key.

## Notes

- CPA's own session affinity, and its preference for Codex credentials with websockets enabled, do not apply to the requests this plugin routes.
- Learned resets and usage live in memory and are relearned after a restart.
- To support another provider, add a reader for its quota headers to `quotaReaders` in `router.go`.
