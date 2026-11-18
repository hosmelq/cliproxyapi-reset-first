# reset-first

A CLIProxyAPI v8.0.69 scheduler plugin that spends the subscription quota that resets soonest first. It supports Claude and Codex subscriptions.

- Learns each subscription's 5-hour and weekly usage and resets from upstream headers: `anthropic-ratelimit-unified-5h-*` and `anthropic-ratelimit-unified-7d-*` for Claude, and the `x-codex-primary-*` and `x-codex-secondary-*` windows for Codex.
- Sends every request to the available subscription whose weekly quota resets soonest. Subscriptions whose reset is still unknown are tried first, so their next response reveals it.
- Moves a subscription that uses up its 5-hour or weekly limit to the end of the order until that limit resets, then sends requests to it again. If CPA still lets it take requests, it gets one request every five minutes to check the limit, so a limit cleared early, for example by a manual reset, puts it back in the order.
- Learns Claude's weekly Fable 5 limit from `anthropic-ratelimit-unified-7d_oi-*`. Only Fable requests count against it. The plugin recognizes them by `fable` in the model name or, for an alias, by the model an earlier request through it used. An account that uses it up moves to the end of the order for Fable requests until the limit resets, and requests for other models keep using it. The five-minute check applies to this limit too.
- The choice is deterministic, so a conversation stays on one account until that account becomes unavailable or reaches a limit, a subscription that resets sooner becomes available, its quota window ends, or the conversation switches between Fable and other models while the first account in the order has no Fable left.
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

Open **Reset First** from the plugin menu in the management dashboard to see, for each provider, which account requests go to, the order after it, and which accounts are not used right now and why. When Fable requests go to a different account than requests for other models, the page shows both. Each account shows the limits its responses through CPA have reported, such as the 5-hour, weekly and Claude's weekly Fable 5 limits, with how much of each is left and when it resets, and the model and time of the last request it served. Usage from other apps shows up after the account's next request through CPA. The page reads `GET /v0/management/reset-first/status`, which requires the management key. It reuses the dashboard login when "Remember password" is on, and otherwise asks for the key.

## Notes

- CPA's own session affinity, and its preference for Codex credentials with websockets enabled, do not apply to the requests this plugin routes.
- Learned resets and usage live in memory and are relearned after a restart.
- After a rate-limit error, CPA itself pauses the account until the limit resets, and the plugin cannot check it during that pause. After an error from the Fable limit alone, CPA pauses only Fable requests on that account, for up to 30 minutes at a time. Redeeming a manual reset on CPA's quota page also ends the pause. After a reset made elsewhere, end it with **Clear cooldown** on the account's card in CPA's Auth Files page.
- To support another provider, add a reader for its quota headers to `quotaReaders` in `router.go`.
