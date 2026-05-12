# kobo-localsend

A minimal [LocalSend](https://localsend.org/) **v2 receiver** for Kobo e-readers, written in Go.

Send EPUBs, PDFs, and any other file straight from your phone or laptop to your Kobo over Wi-Fi — no USB cable, no cloud, no Calibre, no Dropbox folder dance.

> **Status:** receive-only. Sending files *from* the Kobo is out of scope. The official LocalSend app is built with Flutter and cannot run on Kobo's e-ink + Nickel environment, so this project implements the minimum protocol surface needed to be a discoverable, well-behaved receiver.

---

## Features

- 🔍 **Auto-discoverable** — shows up in the LocalSend app on the same Wi-Fi
- 📦 **LocalSend v2 compatible** — works with the official Android, iOS, Windows, macOS, and Linux clients
- 🔔 **Native Kobo UI** — Nickel toasts on each completed transfer, plus a modal "Stop" dialog so you always know the receiver is running
- 📚 **Library auto-rescan** — when books are received, Nickel rescans automatically and they appear on the home screen
- 🪶 **Single static binary** — no Python, no shared libraries, no runtime to install
- 🧊 **Small** — < 6 MB stripped

---

## How it works

LocalSend v2 has two layers:

1. **Discovery** over UDP multicast on `224.0.0.167:53317` (announce + reply), with an HTTP `register` fallback.
2. **Transfer** over HTTP on TCP `53317`:
   - `prepare-upload` — sender lists files, receiver returns a `sessionId` and per-file tokens
   - `upload?sessionId=…&fileId=…&token=…` — raw file body
   - `cancel` — cleanup

`kobo-localsend` implements exactly that subset (receive-only, auto-accept, plain HTTP).

---

## Requirements

### Build host

- **Go ≥ 1.21**
- `make`, `ssh`, `scp` (Linux/macOS; on Windows use WSL or Git Bash)

### Kobo device

| Component                                                             | Required? | Used for                                  |
| --------------------------------------------------------------------- | --------- | ----------------------------------------- |
| SSH access (via [KOReader] or [KoboRoot.tgz])                         | yes       | deploying & launching the binary          |
| [**NickelDBus**](https://github.com/shermp/NickelDBus) (`qndb`)       | yes       | toasts, "Stop" dialog, library rescan     |
| [**NickelMenu**](https://github.com/pgaskin/NickelMenu)               | recommended | start/stop entries in the Nickel UI       |
| [**FBInk**](https://github.com/NiLuJe/FBInk)                          | optional  | fallback notifications when `qndb` is missing |

[KOReader]: https://github.com/koreader/koreader
[KoboRoot.tgz]: https://www.mobileread.com/forums/showthread.php?t=225030

---

## Building

### Local build (test on your PC first)

```sh
make build
make run            # listens on :53317, drops files into ./downloads
```

Open LocalSend on your phone — the alias `Dev PC` should appear. Send a file and verify it lands in `./downloads/`.

### Cross-compile for Kobo

```sh
make kobo
```

Produces `localsend-recv-kobo` (ARMv7, statically linked).

```sh
file localsend-recv-kobo
# ELF 32-bit LSB executable, ARM, EABI5, statically linked, stripped
```

### Choosing the right architecture for your Kobo

Almost every Kobo released in the last decade is **ARMv7-A**, so the defaults work. Exceptions exist for very old or very new models:

| Kobo model(s)                                                               | `GOARCH` | `GOARM` |
| --------------------------------------------------------------------------- | -------- | ------- |
| Aura, Aura H2O (1/2), Aura One, Glo, Glo HD, Mini, Touch 2.0                | `arm`    | `7`     |
| Clara HD, Clara 2E, Forma, Libra H2O, Libra 2, Sage, Elipsa, Elipsa 2E      | `arm`    | `7`     |
| Original Kobo eReader / Kobo Touch N905 (Freescale i.MX35x)                 | `arm`    | `6`     |
| Future ARMv8/aarch64 firmwares                                              | `arm64`  | —       |

**Detect your Kobo's architecture:**

```sh
ssh root@<kobo-ip> 'uname -m'
# armv7l  → GOARM=7
# armv6l  → GOARM=6
# aarch64 → GOARCH=arm64

ssh root@<kobo-ip> 'cat /proc/cpuinfo | head -20'
```

**Override the build flags from the Makefile:**

```sh
make kobo KOBO_ENV='CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=6'
make kobo KOBO_ENV='CGO_ENABLED=0 GOOS=linux GOARCH=arm64'
```

---

## Installation

### Automatic (recommended)

```sh
export KOBO_HOST=192.168.1.50      # your Kobo's IP

make ping                          # sanity-check SSH
make deploy                        # cross-compile + scp binary
make deploy-nm                     # install NickelMenu start/stop items
```

Restart Nickel for the new menu entries to appear (USB plug + unplug, or `ssh root@<kobo-ip> qndb -m mhRestartNickel` if your build exposes it).

### Manual

```sh
# 1. Cross-compile
make kobo

# 2. Copy the binary
ssh root@<kobo-ip> 'mkdir -p /mnt/onboard/.adds/localsend'
scp localsend-recv-kobo root@<kobo-ip>:/mnt/onboard/.adds/localsend/localsend-recv
ssh root@<kobo-ip> 'chmod +x /mnt/onboard/.adds/localsend/localsend-recv'

# 3. (Optional) NickelMenu integration
cat > /tmp/nm-localsend <<'EOF'
menu_item:main:LocalSend (start):cmd_spawn:quiet:exec /mnt/onboard/.adds/localsend/localsend-recv
menu_item:main:LocalSend (stop) :cmd_spawn:quiet:exec killall -TERM localsend-recv
EOF
scp /tmp/nm-localsend root@<kobo-ip>:/mnt/onboard/.adds/nm/localsend
```

---

## Usage

1. Open NickelMenu → **LocalSend (start)**.
   - Toast: `LocalSend: Active on port 53317`.
   - A modal dialog with a **Stop** button confirms it's running.
2. Open LocalSend on your phone or laptop. Your Kobo (e.g. `Kobo Aura`) shows up — send files.
3. Each completed *session* triggers a toast (`Received: foo.epub` or `Received 5 files from Pixel 7`).
4. If any received files are books (`.epub`, `.kepub`, `.pdf`, `.cbz`, …), the Nickel library is rescanned automatically.
5. Tap **Stop**, or NickelMenu → **LocalSend (stop)**, to shut down. A final toast (`Receiver stopped`) confirms exit.

### Flags

```
-alias       Device name shown in LocalSend 
             (default: auto-detected as "<Model> <last-4-of-serial>" 
             from /mnt/onboard/.kobo/version)
-dir         Destination directory (default: /mnt/onboard/LocalSend)
-no-rescan   Skip Nickel library rescan after receiving books
-no-ui       Daemon mode: no modal dialog, stop with SIGTERM
```

### Makefile targets

| Target           | Description                                  |
| ---------------- | -------------------------------------------- |
| `make build`     | Build for the host machine                   |
| `make kobo`      | Cross-compile for ARMv7 (override via `KOBO_ENV`) |
| `make run`       | Run the local build with sane defaults       |
| `make deploy`    | Cross-compile + `scp` to the Kobo            |
| `make deploy-nm` | Install NickelMenu entries on the Kobo       |
| `make start`     | Launch the receiver remotely (background)    |
| `make stop`      | `killall -TERM` on the Kobo                  |
| `make log`       | `tail -F /tmp/localsend.log` over SSH        |
| `make ssh`       | Open a shell on the Kobo                     |
| `make ping`      | Verify the Kobo is reachable                 |
| `make clean`     | Remove local build artifacts                 |

---

## Limitations & security

This is a deliberately minimal implementation. Read this section before exposing your Kobo to a network you don't trust.

- **Plain HTTP, no TLS** — every byte of every file is unencrypted on the wire.
- **No PIN, auto-accept** — any host on the same L2/multicast domain that can reach UDP `53317` can push files to your Kobo. Fine on a home Wi-Fi; **not** fine on hotel/coffee-shop networks. If your router supports "AP isolation" or "client isolation", it will break discovery (which is what we want elsewhere).
- **FAT32 limit** — `/mnt/onboard` is FAT32; files larger than 4 GiB cannot be stored.
- **Aggressive Wi-Fi sleep** — stock Nickel suspends the Wi-Fi radio after a few minutes of inactivity. While the receiver is running it requests nsForceWifi(true) via NickelDBus, which prevents this on most firmwares. If your firmware ignores it, edit [PowerOptions] ForceWifiOn=true in Kobo eReader.conf (requires a Nickel restart and applies permanently, with battery cost).
- **Modal dialog blocks Nickel UI** — while the "Stop" dialog is shown you can't navigate to a book. Use `-no-ui` if you want the receiver to live alongside reading.

---

## Non-goals

To keep the project small, focused, and easy to audit, the following are **explicitly out of scope**. PRs implementing them will likely be declined.

- **Sending files from the Kobo.** This is a receive-only daemon. Composing a sender would require a real touch UI, file picker, and progress feedback — none of which are practical on Nickel without replacing the launcher (which is what KOReader/Plato already do).
- **TLS / HTTPS transport.** LocalSend's optional TLS uses self-signed certificates that the official clients pin per-device. Implementing it correctly adds complexity for marginal benefit on a trusted home LAN. Use a trusted network instead.
- **PIN authentication / per-transfer prompts.** Auto-accept is intentional: the whole point is \"tap launch, send, done\". If you need confirmation, you already have it — the sender side prompts before transferring.
- **Replacing Nickel or providing a full-screen UI.** The receiver coexists with Nickel; it does not take over the device. If you want a full reading-and-receiving experience, use KOReader.
- **Cross-platform builds (Windows / macOS / generic Linux desktop) as first-class targets.** The code happens to compile and run anywhere Go runs, and that's useful for local testing — but the project is shaped around Kobo. Other platforms get no Nickel UI, no library rescan, and no support.
- **LocalSend protocol versions other than v2.** v1 is legacy; future versions will be adopted only if they remain backwards-compatible with v2 receivers.
- **Multi-interface / VPN / bridged-network heroics.** The receiver binds to all interfaces and trusts the OS routing. If your network topology is exotic, it's on you.
- **Packaging for non-Kobo e-readers** (Kindle, PocketBook, Boox, reMarkable). The Nickel-specific UI glue (`qndb`, NickelMenu) wouldn't apply, and supporting other ecosystems would dilute the scope. Forks are welcome.

## Troubleshooting

<details>
<summary><b>The sender doesn't see my Kobo</b></summary>

Both devices must be on the same Wi-Fi subnet and the network must allow UDP multicast to `224.0.0.167`. AP/client isolation breaks discovery. As a workaround, the LocalSend app lets you "Add device manually" by IP.
</details>

<details>
<summary><b><code>qndb dlg: exit status 1 (non-existent method or invalid parameter count)</code></b></summary>

Your NickelDBus build exposes a different signature for the dialog API. Inspect the available methods:

```sh
dbus-send --system --print-reply \
  --dest=com.github.shermp.nickeldbus /nickeldbus \
  org.freedesktop.DBus.Introspectable.Introspect \
  | grep -A2 dlgConfirm
```

Adapt `showControlDialog()` in `main.go` to one of the available `dlgConfirm*` methods.
</details>

<details>
<summary><b>Files arrive but don't show in the library</b></summary>

Confirm the extension is recognized in the `bookExts` map and that `qndb` is installed. Manually trigger a rescan:

```sh
ssh root@<kobo-ip> 'qndb -m pfmRescanBooks'
```
</details>

<details>
<summary><b><code>make deploy</code> fails with SSH key-exchange errors</b></summary>

Older Kobos ship with a Dropbear that doesn't negotiate modern key algorithms. Append to `SSH_OPTS` in the `Makefile`:

```
SSH_OPTS += -oHostKeyAlgorithms=+ssh-rsa -oPubkeyAcceptedKeyTypes=+ssh-rsa
```
</details>

---

## Project layout

```
.
├── main.go      # HTTP server + UDP discovery + Nickel UI glue
├── go.mod
├── Makefile     # build / deploy / start / stop / log
└── README.md
```

---

## Acknowledgements

- [LocalSend](https://localsend.org/) by Tien Do Nam et al. — the protocol and the lovely cross-platform clients
- [NickelDBus](https://github.com/shermp/NickelDBus) by Sherman Perry
- [FBInk](https://github.com/NiLuJe/FBInk) by NiLuJe
- [NickelMenu](https://github.com/pgaskin/NickelMenu) by Patrick Gaskin

## License

MIT — see [`LICENSE`](LICENSE).