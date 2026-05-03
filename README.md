# denon

> 📖 **The story:** [I Bought a Denon for My Wedding. Twelve Years Later, I Vibe-Coded Its Radio Back to Life.](https://victorantos.com/posts/i-bought-a-denon-for-my-wedding-then-i-vibe-coded-its-radio-back/)

A self-hosted replacement for the discontinued **vTuner** internet-radio directory baked into Denon, Marantz, Yamaha, Onkyo, and Pioneer AVRs from roughly 2011 to 2017.

If your receiver's "Internet Radio" menu went silent — empty list, "service unavailable," or a vTuner ad about a $3/year subscription — this brings it back. Your hardware was never broken; the directory server it phoned home to was retired. We host one on your LAN instead.

## Quick install

On the always-on machine on your network — Mac, Linux box, Raspberry Pi, NAS that runs containers, anything that stays awake:

```sh
curl -fsSL https://raw.githubusercontent.com/victorantos/denon/main/install.sh | sh
```

That detects your OS/architecture, downloads the right binary from the latest GitHub release, auto-detects your LAN IP, and installs a system service (LaunchDaemon on macOS, systemd unit on Linux) that survives reboots.

Then, on the receiver:

1. Settings → Network → DNS → **Manual**
2. Primary DNS: *the IP of the machine you just installed on*
3. Secondary DNS: *same IP*
4. Reboot the receiver
5. Open Internet Radio

The menu populates with **Most Popular**, **Genres**, **Countries**, **Languages**, and **Search**, all backed by [radio-browser.info](https://www.radio-browser.info)'s ~50,000-station community database. Tap a station; it plays.

## What it does

The receiver thinks it's still talking to vTuner. It isn't.

```
Receiver  ─DNS query for denon.vtuner.com──►  denon (your LAN)
           ─HTTP GET /setupapp/Denon/...───►  denon
                                              ↓
                                              radio-browser.info
                                              ↓
           ◄──vTuner-format XML directory────  denon
           ─HTTP GET /play?id=UUID─────────►  denon
           ◄──302 to actual stream URL──────  denon
```

A single Go binary speaks both protocols:

- **DNS server** on UDP :53 — intercepts `*.vtuner.com`, `radiodenon.com`, `radiomarantz.com`, `radiosetup.com`, `*.my-noxon.net` and answers with its own IP. Everything else is forwarded upstream.
- **HTTP server** on TCP :80 — speaks the vTuner XML protocol the firmware was built around.

No mock cloud, no third-party subscription, no patched firmware.

## Compatibility

Confirmed model paths covered by the default DNS intercepts:

| Brand | Models | Hostname pattern |
|---|---|---|
| Denon | AVR-X1000–X5000 series, AVR-1xxx/2xxx/3xxx/4xxx (2011–2017) | `denon.vtuner.com`, `radiodenon.com` |
| Marantz | NR-series, SR-series of the same era | `*.vtuner.com`, `radiomarantz.com` |
| Yamaha | RX-V, RX-A pre-MusicCast | `*.vtuner.com` |
| Onkyo / Pioneer | most pre-2018 | `*.vtuner.com`, `radiosetup.com` |

If your receiver hits a hostname not in the default list, add it with `-dns-intercept`. Open an issue with `tcpdump -i any port 53` from your network — happy to expand the defaults.

## Older receivers stuck in "vTuner unavailable" state

Some receivers (verified on the Denon AVR-X3000) have been sitting for years with vTuner reachable-but-empty, and their firmware has cached a "service unavailable" state. The on-screen Internet Radio menu in that state shows **only the local Favorites list** — no Most Popular, no Genres, no Search. Pressing Internet Radio doesn't trigger a fresh directory fetch.

This service handles that case by:

1. **Stream proxy on `/play`** — converts radio-browser's HTTPS streams to plain HTTP for receivers that can't do TLS (most pre-2017 hardware can't follow HTTPS redirects on the playback path).
2. **Legacy-favorites handler on `/setupapp/<vendor>/asp/func/dynamOD.asp`** — catches the receiver's "play this saved-Favorite ID" requests, hashes the cached numeric ID into a list of currently-popular HTTP MP3 stations, and proxies whichever station that hash picks. Same Favorite always plays the same station; different Favorites play different stations.

The receiver's *display* will still show the labels saved years ago ("BBC Radio 2 live", "Sky.fm Compact Discoveries", whatever) because those labels live in the receiver's NVRAM. The audio underneath each label is whatever popular station the hash picked.

**Workaround for live menu browsing on stuck receivers:** every Denon and Marantz of this era shipped with a hidden web UI at `http://<receiver-ip>/NetAudio/index.html`. The "Search by Keyword" field there triggers a live search request through us against radio-browser, returning playable results. Useful when the on-screen menu refuses to update.

A **Network Settings reset** on the receiver (Setup → General → Reset → Network Settings; *not* Default Settings) sometimes unsticks the on-screen menu without a firmware update — at the cost of having to re-enter your static IP/DNS config. Worth trying before you give up on the on-screen menu.

**Don't update firmware** to fix this. Denon discontinued AVR-X3000 firmware updates in mid-2023, and even the final 2023 firmware doesn't have logic to handle the stuck-vTuner case (Denon never wrote it). Updating buys nothing and risks bricking.

## Configuration

Everything is flags. The installer picks sensible defaults; pass them yourself if you want to.

```
denon -h
  -http :80                listen address for the HTTP server
  -dns :53                 listen address for the DNS server (empty disables DNS)
  -base-url URL            URL the receiver sees us at (auto-detected)
  -intercept-ip IP         IP returned for intercepted A queries
  -dns-upstream a,b,c      forwarders for non-intercepted queries
  -dns-intercept a,b,c     hostname suffixes to intercept
  -v                       verbose logging
```

If you already run Pi-hole or similar, set `-dns ""` to disable the bundled DNS server and add the intercepts as DNS overrides on your existing resolver.

## Building from source

Requires Go 1.22+.

```sh
git clone https://github.com/victorantos/denon
cd denon
make build           # native binary in bin/
make build-all       # cross-compile to dist/ for darwin/linux/windows × amd64/arm64
make install         # install from local build
make uninstall
make logs            # tail service logs
make status
```

## Troubleshooting

**Menu doesn't populate.** Tail the logs (`tail -f /var/log/denon.*.log` on macOS, `journalctl -fu denon` on Linux). If you don't see DNS hits and HTTP hits within ~15 seconds of opening Internet Radio on the receiver, the receiver isn't reaching the server. Confirm the receiver's DNS is pointed at the host's IP and that no firewall blocks ports 53/UDP and 80/TCP.

**Server's IP changed.** Pin it with a DHCP reservation (or static IP). The receiver caches the DNS server you set, so its DNS still works, but the cached A records the receiver got back point at an IP that may now be someone else.

**Some stations don't play.** radio-browser.info is community-curated; quality varies. Stations with `lastcheckok=0` are filtered out, but a stream can go down between scans. Try another.

## Why this exists

I bought a Denon AVR-X3000 in 2014, weeks before getting married. Twelve years later it still produces beautiful sound. The firmware is frozen — Denon stopped updates in mid-2023 — but the hardware doesn't care.

Then the radio stopped working. Not because anything broke, but because the directory server it called home to was retired.

This is the fix. About 900 lines of Go, one weekend, and your radio plays again.

## Credits

[YCast](https://github.com/milaq/YCast) and [YTuner](https://github.com/coffeegreg/YTuner) reverse-engineered the protocol first; this is a clean-room re-implementation written against their docs and observed behavior. Stations come from [radio-browser.info](https://www.radio-browser.info), a community-run open station database.

## License

MIT. See [LICENSE](LICENSE).
