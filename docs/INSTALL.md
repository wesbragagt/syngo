# Installing syncthis

One binary per device: the CLI and the daemon are the same program. Every
install below ends the same way, with a daemon that has an identity, a data
directory it owns, and a listen address peers can reach.

The flake is the supported install path. The other paths are the same binary
and the same unit, packaged by hand.

Windows is not supported.

Every command here was run on the machine that wrote this document, x86_64
NixOS 25.11 with Nix 2.31, Go 1.26.7 and Docker 28.5, unless the section says
otherwise. Where a command could not be run here, the reason is stated where
it appears.

- [NixOS through the flake](#nixos-through-the-flake)
- [Home-manager](#home-manager)
- [Linux without Nix](#linux-without-nix)
- [macOS](#macos)
- [NAS and headless servers](#nas-and-headless-servers)
- [Container](#container)
- [Configure](#configure)
- [Run and verify](#run-and-verify)

## NixOS through the flake

Add the flake as an input and import the NixOS module.

This repository is not published yet: it has no git remote, so there is no
`github:` URL to fetch and every snippet below points at a local checkout
instead. Clone it somewhere and use that path. Once the repository is
published, `syncthis.url = "github:wesbragagt/syncthis";` replaces the `path:`
line in every snippet on this page and nothing else changes.

```nix
{
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    syncthis.url = "path:/home/you/src/syncthis";
  };

  outputs = { self, nixpkgs, syncthis }: {
    nixosConfigurations.laptop = nixpkgs.lib.nixosSystem {
      system = "x86_64-linux";
      modules = [
        syncthis.nixosModules.default
        {
          services.syncthis = {
            enable = true;
            interface = "tailscale0";
          };
        }
      ];
    };
  };
}
```

Then build the system the way you always do:

```console
$ sudo nixos-rebuild switch --flake .#laptop
```

Not run here: switching this machine's real system configuration is not
something a documentation change should do. What was run instead is this
flake's own check, a NixOS system that imports the module and sets
`interface = "tailscale0"`, built to completion, which is everything short of
activating it:

```console
$ nix build .#checks.x86_64-linux.nixos-module --no-link --print-out-paths
/nix/store/dp6q7ffbhb74q53jni5a57s2f0ka4c3r-nixos-system-nixos-26.11.20260919.20b1ddd
```

`nix flake check` runs that one and additionally boots the module in two VMs.

The module adds `syncthis` to `environment.systemPackages`, creates the
`syncthis` user and group, creates `/var/lib/syncthis` mode 0700 owned by that
user through `systemd.tmpfiles`, and defines `syncthis.service`.

### Options

Defined in `nix/service.nix` and `nix/nixos-module.nix`.

| Option | Default | Meaning |
|---|---|---|
| `services.syncthis.enable` | `false` | run the daemon |
| `services.syncthis.package` | this flake's `syncthis` | the package to run |
| `services.syncthis.dataDir` | `/var/lib/syncthis` | passed as `SYNCTHIS_DATA_DIR` |
| `services.syncthis.interface` | `null` | passed as `daemon run --interface`; `null` keeps the listen address from `config.json` |
| `services.syncthis.restart` | `"always"` | systemd `Restart=` |
| `services.syncthis.restartSec` | `5` | seconds between restarts |
| `services.syncthis.user` | `"syncthis"` | the user the daemon runs as |
| `services.syncthis.group` | `"syncthis"` | the group the daemon runs as |
| `services.syncthis.createUser` | `true` | declare that user and group here |

The daemon's user must be able to read and write every synchronized folder. If
the folders belong to your login user, set `services.syncthis.user` to it and
`createUser = false`.

The module opens no firewall port. Which port the daemon listens on comes from
its own `config.json`, so opening one is your decision:
`networking.firewall.allowedTCPPorts = [ 22000 ];`. On a tailnet nothing has to
be opened.

### Give the daemon an identity

The module cannot generate the identity: the private key is state, not
configuration. Run `init` once, as the daemon's user, with the same data
directory the unit uses.

```console
$ sudo -u syncthis env SYNCTHIS_DATA_DIR=/var/lib/syncthis \
    syncthis init --name laptop --listen 0.0.0.0:22000
initialized /var/lib/syncthis
device id: IRGAXGLUF4X5OSDVNADDCDXI4K5T476EK6E4A3WGFYMP2FMBOJFQ
device name: laptop
listen: 0.0.0.0:22000
```

(The id comes from a run on a scratch data directory on this machine, and the
first line named that directory rather than `/var/lib/syncthis`; your id
differs.)

With `services.syncthis.interface = "tailscale0"` the host part of `--listen`
is replaced at start by the interface's address, so `0.0.0.0:22000` here only
picks the port.

### Before init the unit restarts

This is the documented first-run state, not a fault. With no identity the
daemon exits 3 and `Restart=always` brings it back, so the unit loops instead
of failing, and it comes up on its own as soon as `init` has run.

```console
$ syncthis --data-dir /tmp/sdemo/empty daemon run
syncthis: device identity not initialized: /tmp/sdemo/empty/identity.key missing
$ echo $?
3
```

The journal carries the same line, naming the missing file:

```console
$ systemctl show syncthis.service -p ExecMainStatus --value
3
$ journalctl -u syncthis.service | grep 'identity'
... syncthis: device identity not initialized: /var/lib/syncthis/identity.key missing
```

`nix/vm-tests.nix` asserts exactly this: exit status 3, the message
`device identity not initialized`, the path `identity.key missing`, and the
unit going active after `init`.

### Without a flake input

To try the binary without adding an input, run the checkout:

```console
$ nix run path:/home/you/src/syncthis -- version
syncthis dev (dev, go1.26.7)
```

A `path:` flake carries no git revision, so the version stamp is `dev`. Point
Nix at the git tree instead and it stamps the commit:

```console
$ nix run "git+file:///home/you/src/syncthis" -- version
syncthis 6e76ddb-dirty (6e76ddb, go1.26.7)
```

That is HEAD of the checkout, with the `-dirty` suffix because the tree it was
run against had uncommitted changes, the same suffix `git describe --dirty`
adds. Add `?ref=<branch>` to pick a branch.

Once the repository is published this is `nix run github:wesbragagt/syncthis
-- version`.

## Home-manager

The home-manager module runs the same daemon as a systemd **user** service.

Not covered by any test. `nix flake check` does not evaluate it:
`homeManagerModules` is not an output Nix knows, so it is only warned about and
skipped, and there is no home-manager input in this flake to evaluate it
against. The NixOS module, which shares `nix/service.nix` with it, is covered
by two VM tests. Read the home-manager module as untested until someone runs it
under real home-manager.

```nix
{
  inputs.syncthis.url = "path:/home/you/src/syncthis";

  # inside your home-manager configuration
  imports = [ syncthis.homeManagerModules.default ];
  services.syncthis = {
    enable = true;
    interface = "tailscale0";
  };
}
```

The options are the ones in the table above, minus `user`, `group` and
`createUser`. `dataDir` defaults to `${config.xdg.dataHome}/syncthis`.

Then, once:

```console
$ syncthis init --name laptop
$ loginctl enable-linger $USER     # keep the daemon running when logged out
$ systemctl --user status syncthis
```

## Linux without Nix

Build the archives on any machine with Go 1.26:

```console
$ make dist
building syncthis_6e76ddb_linux-amd64
building syncthis_6e76ddb_linux-arm64
building syncthis_6e76ddb_linux-armv7
building syncthis_6e76ddb_darwin-amd64
building syncthis_6e76ddb_darwin-arm64
-rw-r--r-- 1 wesbragagt users     512 Sep 22 12:10 SHA256SUMS
-rw-r--r-- 1 wesbragagt users 9809703 Sep 22 12:10 syncthis_6e76ddb_darwin-amd64.tar.gz
-rw-r--r-- 1 wesbragagt users 9351724 Sep 22 12:10 syncthis_6e76ddb_darwin-arm64.tar.gz
-rw-r--r-- 1 wesbragagt users 9658527 Sep 22 12:10 syncthis_6e76ddb_linux-amd64.tar.gz
-rw-r--r-- 1 wesbragagt users 9002715 Sep 22 12:10 syncthis_6e76ddb_linux-arm64.tar.gz
-rw-r--r-- 1 wesbragagt users 9034334 Sep 22 12:10 syncthis_6e76ddb_linux-armv7.tar.gz
```

The version in every file name is what `git describe --tags --always --dirty`
printed on the build machine, `6e76ddb` in the run above. Yours differs, so
where a command below says `<version>`, substitute the one your `dist`
directory actually holds.

Every binary is `CGO_ENABLED=0` and `-trimpath`, so it has no dynamic
interpreter and runs on a machine other than the one that built it.
`make dist-verify` checks the digests, extracts every archive, checks each
binary is static and built for the platform its name claims, and runs the one
for this machine:

```console
$ make dist-verify
sha256 ok   syncthis_6e76ddb_darwin-amd64.tar.gz
sha256 ok   syncthis_6e76ddb_darwin-arm64.tar.gz
sha256 ok   syncthis_6e76ddb_linux-amd64.tar.gz
sha256 ok   syncthis_6e76ddb_linux-arm64.tar.gz
sha256 ok   syncthis_6e76ddb_linux-armv7.tar.gz
extract ok  syncthis_6e76ddb_linux-amd64
static ok   dist/verify/syncthis_6e76ddb_linux-amd64/syncthis (elf EM_X86_64)
version ok  syncthis_6e76ddb_linux-amd64: syncthis 6e76ddb (6e76ddb, go1.26.7)
extract ok  syncthis_6e76ddb_linux-arm64
static ok   dist/verify/syncthis_6e76ddb_linux-arm64/syncthis (elf EM_AARCH64)
extract ok  syncthis_6e76ddb_linux-armv7
static ok   dist/verify/syncthis_6e76ddb_linux-armv7/syncthis (elf EM_ARM)
extract ok  syncthis_6e76ddb_darwin-amd64
static ok   dist/verify/syncthis_6e76ddb_darwin-amd64/syncthis (mach-o CpuAmd64)
extract ok  syncthis_6e76ddb_darwin-arm64
static ok   dist/verify/syncthis_6e76ddb_darwin-arm64/syncthis (mach-o CpuArm64)
dist-verify: all artifacts ok
```

On the target machine, check the digest and install the binary:

```console
$ sha256sum -c --ignore-missing SHA256SUMS
$ tar -xzf syncthis_<version>_linux-amd64.tar.gz
$ sudo install -m 0755 syncthis /usr/local/bin/syncthis
$ syncthis version
```

`/usr/local/bin/syncthis` is the path `packaging/systemd/syncthis.service` and
the launchd plist both name. Install it elsewhere and edit the unit.

Then the identity and the user unit:

```console
$ syncthis init --name laptop
$ mkdir -p ~/.config/systemd/user
$ cp packaging/systemd/syncthis.service ~/.config/systemd/user/
$ systemctl --user daemon-reload
$ systemctl --user enable --now syncthis
$ systemctl --user is-active syncthis
active
```

The unit runs as the invoking user, with
`SYNCTHIS_DATA_DIR=%h/.local/share/syncthis`, `Restart=always`,
`RestartSec=5`, `KillSignal=SIGTERM` and `StartLimitIntervalSec=0`. Nothing in
it needs root. Change the data directory or add `--interface` with
`systemctl --user edit syncthis`. Add `loginctl enable-linger $USER` to keep
the daemon running when that user is not logged in.

Partly untested here. NixOS has no `/usr/local/bin`, so `install` into it was
not run. The unit itself was started on this machine under a scratch name,
with `ExecStart` pointed at the Nix store binary and `SYNCTHIS_DATA_DIR`
pointed at a scratch directory; systemd reported `active (running)`.

## macOS

Not tested here: this is a Linux machine, so no launchd command below was run.
The `darwin-amd64` and `darwin-arm64` archives are built and inspected by
`make dist-verify` above, which checks the Mach-O header and the architecture
but cannot run the binary.

Take `syncthis_<version>_darwin-arm64.tar.gz` for Apple silicon,
`darwin-amd64` for Intel.

```console
$ shasum -a 256 -c SHA256SUMS
$ tar -xzf syncthis_<version>_darwin-arm64.tar.gz
$ sudo install -m 0755 syncthis /usr/local/bin/syncthis
$ syncthis init --name macbook
```

The plist is a LaunchAgent: it runs as the user who loads it, so the daemon
owns the data directory and the files it synchronizes. launchd does not expand
`~` or `$HOME` there, so the plist carries absolute paths with `USER` standing
in for your short name. Substitute it while copying:

```console
$ mkdir -p ~/Library/LaunchAgents ~/Library/Logs
$ sed "s/USER/$(id -un)/g" packaging/launchd/com.github.wesbragagt.syncthis.plist \
    > ~/Library/LaunchAgents/com.github.wesbragagt.syncthis.plist
$ launchctl load -w ~/Library/LaunchAgents/com.github.wesbragagt.syncthis.plist
$ launchctl list | grep syncthis
```

`USER` appears four times: in `SYNCTHIS_DATA_DIR`
(`/Users/USER/Library/Application Support/syncthis`), `WorkingDirectory`,
`StandardOutPath` and `StandardErrorPath`. The data directory there is not the
`~/.local/share/syncthis` default, so pass `--data-dir` to `init` if you want
the CLI and the agent to share one:

```console
$ syncthis --data-dir "$HOME/Library/Application Support/syncthis" init --name macbook
```

or export `SYNCTHIS_DATA_DIR` in your shell. Logs go to
`~/Library/Logs/syncthis.log`. To stop it:

```console
$ launchctl unload -w ~/Library/LaunchAgents/com.github.wesbragagt.syncthis.plist
```

## NAS and headless servers

A 64-bit ARM box (most recent NAS boxes, Raspberry Pi 3 and later on a 64-bit
OS) takes `linux-arm64`. A 32-bit ARM box takes `linux-armv7`, which is built
with `GOARM=7`. Check with `uname -m`: `aarch64` means arm64, `armv7l` means
armv7.

```console
$ tar -xzf syncthis_<version>_linux-armv7.tar.gz
$ sudo install -m 0755 syncthis /usr/local/bin/syncthis
```

Not run here: this machine is x86_64, so neither ARM binary was executed.
`make dist-verify` confirmed the ELF machine types (`EM_AARCH64`, `EM_ARM`) and
that both are static.

A headless box has no logged-in user, so run the daemon as a system service.
Give it its own user, a data directory it owns, and the folders it syncs:

```console
$ sudo useradd --system --home-dir /var/lib/syncthis --create-home syncthis
$ sudo install -d -m 0700 -o syncthis -g syncthis /var/lib/syncthis
$ sudo install -d -m 0755 -o syncthis -g syncthis /srv/docs
$ sudo -u syncthis env SYNCTHIS_DATA_DIR=/var/lib/syncthis \
    syncthis init --name nas --listen 0.0.0.0:22000
```

Not run here: `useradd` is not how NixOS declares users, and creating a system
user, `/var/lib/syncthis` and `/srv/docs` on this machine would leave state
behind that no configuration owns. The NixOS module does all four, and the VM
tests boot it and assert the data directory is mode 0700 owned by
`syncthis:syncthis`. `syncthis init --name nas --listen 0.0.0.0:22000` itself
was run here against a scratch data directory and printed
`listen: 0.0.0.0:22000`.

Then a system unit, which is the user unit in `packaging/systemd/` plus the
three lines a system service needs:

```console
$ sudo cp packaging/systemd/syncthis.service /etc/systemd/system/
$ sudo systemctl edit --full syncthis    # set the three lines below
$ sudo systemctl daemon-reload
$ sudo systemctl enable --now syncthis
```

```ini
[Service]
User=syncthis
Group=syncthis
Environment=SYNCTHIS_DATA_DIR=/var/lib/syncthis
```

and change `[Install] WantedBy=default.target` to `multi-user.target`, so it
starts at boot without a session. On a tailnet add `--interface tailscale0` to
`ExecStart`. This is what the NixOS module builds for you; on NixOS use the
module instead.

Not tested here: this machine's units are generated by NixOS, so no unit was
installed into `/etc/systemd/system`. The equivalent NixOS unit was booted and
exercised by `nix flake check`.

## Container

The image is built from `packaging/container/Dockerfile`:

```console
$ make container IMAGE=syncthis:docs-test
 => naming to docker.io/library/syncthis:docs-test
```

It runs as the user `syncthis`, uid 1000, with `SYNCTHIS_DATA_DIR=/data` and
`ENTRYPOINT ["/usr/local/bin/syncthis"]`, so arguments to `docker run` are
syncthis arguments. Two volumes: `/data` is the identity and the database,
`/folders` the synchronized trees. A bind-mounted host directory must be
readable and writable by uid 1000.

Give it an identity first. The identity lives in the `/data` volume, so this is
one `docker run` with the same mount the daemon gets:

```console
$ mkdir -p /tmp/sdemo/c/data /tmp/sdemo/c/folders/docs
$ docker run --rm -v /tmp/sdemo/c/data:/data -v /tmp/sdemo/c/folders:/folders \
    syncthis:docs-test init --name box
initialized /data
device id: JRNOBFID54R57OO5SN2ABC4FWLPPVV6IUNHE4R4XWX2OSKHPFICQ
device name: box
listen: 127.0.0.1:22000
```

The daemon refuses a public bind without `--allow-public`, and a wildcard bind
counts as public, so give it the container's own interface:

```console
$ docker run -d --name syncthis -v /tmp/sdemo/c/data:/data \
    -v /tmp/sdemo/c/folders:/folders -p 22003:22000 \
    syncthis:docs-test daemon run --interface eth0
$ docker logs syncthis
time=2026-09-22T17:11:37.964Z level=INFO msg="peer listener bound" address=172.17.0.2:22000
time=2026-09-22T17:11:37.964Z level=INFO msg="listening for peers" address=172.17.0.2:22000
time=2026-09-22T17:11:37.964Z level=INFO msg="bandwidth limits applied" upload=unlimited download=unlimited
time=2026-09-22T17:11:37.964Z level=INFO msg="daemon started" folders=0 rescan_interval=10m0s
syncthis daemon running (pid 1, data dir /data)
```

The daemon is pid 1, so `docker stop` sends it SIGTERM, which is what it shuts
down cleanly on. Every other command goes into the running container, where it
reaches the control socket in `/data`:

```console
$ docker exec syncthis syncthis status --json | jq -c .daemon
{"running":true,"pid":1,"listen":"172.17.0.2:22000","started_at":"2026-09-22T17:11:37Z","upload_limit":0,"download_limit":0}
$ docker exec syncthis syncthis folder add /folders/docs --id docs
added folder docs at /folders/docs
```

The directory has to exist inside the container before `folder add`: the
command exits 2 on a path that is not a directory, which is why the `mkdir`
above created `/tmp/sdemo/c/folders/docs`.

A peer outside the container reaches it at the published port, `22003` above.
The address the daemon binds is the container's, so tell the peer the host's
address and port: `syncthis peer add <host>:22003`.

## Configure

### Data directory

Resolved from `--data-dir`, else `$SYNCTHIS_DATA_DIR`, else
`$XDG_DATA_HOME/syncthis`, else `~/.local/share/syncthis`. Every install above
sets it explicitly for the service, so set the same one for the CLI, or the CLI
talks to a different device with no daemon behind it.

It holds `config.json`, `identity.key` (mode 0600), `identity.crt`, `state.db`,
and, while the daemon runs, `control.sock` (mode 0600) and `daemon.pid`. One
daemon owns one data directory: a second exits 6. Back up `identity.key` and
`identity.crt`: the device id is that key, and losing it means every peer has
to trust a new id. `config export` never includes them. Full contents in
[STATE.md](STATE.md#data-directory).

### Settings worth changing

`syncthis config show` prints them, `syncthis config set` writes them and asks
a running daemon to reload.

| Setting | Why you would change it |
|---|---|
| `listen` | the address peers connect to; the port is the part that matters when an interface is set |
| `interface` | bind this interface's first IPv4 address instead of the host part of `listen` |
| `upload_limit`, `download_limit` | bytes per second, `0` for unlimited; the only two that take effect without a restart |
| `rescan_interval` | how often a folder is fully rescanned, 10m by default |
| `sync_interval` | how often a connected peer is asked what changed, 30s by default |
| `log_level` | `debug` logs paths; `info` does not |

`chunk_size` and `max_file_size` are read from `config.json` and have no
`config set` flag: edit the file and restart the daemon. `max_file_size` is the
largest file this device accepts from a peer, 1 TiB by default; a manifest
announcing a bigger one is refused whole.

```console
$ syncthis config set --download-limit 4MiB
$ syncthis config set --interface tailscale0
```

### The interface rule on a tailnet

Without `--allow-public` the resolved address must be loopback, link-local,
RFC 1918 private, or in the Tailscale range `100.64.0.0/10`. A wildcard bind
such as `0.0.0.0:22000` counts as public and is refused with exit 2.

So on a tailnet, bind the interface and never pass `--allow-public`:

```console
$ syncthis daemon run --interface tailscale0
```

The daemon then listens on the device's `100.64.0.0/10` address, which only the
tailnet can reach. No firewall port is opened. Set it once with
`syncthis config set --interface tailscale0`, or with
`services.syncthis.interface = "tailscale0"` under NixOS, and `daemon run`
needs no flag. Check the interface first:

```console
$ syncthis network tailscale detect
$ syncthis network peers
```

### Where folder state lives

Two directories per synchronized folder, both inside it:

- `<folder>/.syncthis/tmp/` holds partial downloads, so a finished file is
  renamed onto its target without crossing a filesystem boundary. The scanner
  always ignores `.syncthis`. `syncthis repair` removes partial files no
  unfinished transfer claims.
- `<folder>/.syncthisignore` is this device's ignore patterns. It is never
  scanned, never sent to a peer and never accepted from one.

Everything else, the file table, the transfer journal and the conflicts, is in
`state.db` in the data directory. Deleting either directory is safe and costs a
rescan or a re-download. See [STATE.md](STATE.md).

## Run and verify

Every command, every flag and every JSON field is in [CLI.md](CLI.md). This is
the shortest path from an installed binary to two devices that agree.

The walkthrough below runs two devices on one machine, each with its own data
directory, folder and port, so it is reproducible without a second machine.
Every block was run in order, exactly as written, with `syncthis` on `PATH`.
For two real devices, drop `--data-dir` and run each block on its own machine;
the differences are called out where they matter.

### 1. Start the daemon

Give each device an identity and a data directory first. On a real device this
is one `syncthis init`, and the service you installed above starts the daemon;
here it is two of each.

```console
$ mkdir -p /tmp/sdemo/a/data /tmp/sdemo/a/docs /tmp/sdemo/b/data /tmp/sdemo/b/docs
$ syncthis --data-dir /tmp/sdemo/a/data init --name laptop --listen 127.0.0.1:22001
initialized /tmp/sdemo/a/data
device id: PQXYNPE2LEFKUFHG2DQCSTKYZ5NZ4XDTTNNKEOCWFTMMYNO3PJSQ
device name: laptop
listen: 127.0.0.1:22001
$ syncthis --data-dir /tmp/sdemo/b/data init --name nas    --listen 127.0.0.1:22002
initialized /tmp/sdemo/b/data
device id: JI7HZGFPUXQ5RYPY6CA6Y3J6772RRQD6VDL3UILRIHUL6J2M2XEA
device name: nas
listen: 127.0.0.1:22002
```

Keep both device ids: step 2 pins them. Then start the daemons. On a real
device this is the service instead:

```console
$ systemctl --user start syncthis          # user unit
$ sudo systemctl start syncthis            # system unit, NixOS module included
```

```console
$ syncthis --data-dir /tmp/sdemo/a/data daemon run > /tmp/sdemo/a.log 2>&1 &
$ syncthis --data-dir /tmp/sdemo/b/data daemon run > /tmp/sdemo/b.log 2>&1 &
$ syncthis --data-dir /tmp/sdemo/a/data status --json | jq .daemon.running
true
```

`daemon.running` is `true` when the daemon answers its control socket. A
`status` that exits 3 means this data directory has no identity yet.

### 2. Pair two devices

Each side runs `peer add` once, naming the other. Pairing is done when both
sides have run it.

```console
$ syncthis --data-dir /tmp/sdemo/a/data peer add 127.0.0.1:22002 --yes
device id: JI7HZGFPUXQ5R-YPY6CA6Y3J677-2RRQD6VDL3UIL-RIHUL6J2M2XEA
name:      nas
added peer nas (JI7HZGFPUXQ5R-YPY6CA6Y3J677-2RRQD6VDL3UIL-RIHUL6J2M2XEA) at 127.0.0.1:22002
the peer has not added this device yet; run `syncthis peer add` there too
$ syncthis --data-dir /tmp/sdemo/b/data peer add 127.0.0.1:22001 --yes
device id: PQXYNPE2LEFKU-FHG2DQCSTKYZ5-NZ4XDTTNNKEOC-WFTMMYNO3PJSQ
name:      laptop
added peer laptop (PQXYNPE2LEFKU-FHG2DQCSTKYZ5-NZ4XDTTNNKEOC-WFTMMYNO3PJSQ) at 127.0.0.1:22001
```

The last line of the first command is the expected state until the other side
has run its `peer add`.

On a tailnet, name the device instead of an address and pin the id `init`
printed on it:

```console
laptop$ syncthis peer add nas    --device-id JI7HZGFPUXQ5RYPY6CA6Y3J6772RRQD6VDL3UILRIHUL6J2M2XEA
nas$    syncthis peer add laptop --device-id PQXYNPE2LEFKUFHG2DQCSTKYZ5NZ4XDTTNNKEOCWFTMMYNO3PJSQ
```

`syncthis status --json | jq -r .device.id` prints the id each side pins. A
bare host name is looked up through the local Tailscale client and the address
that answers is stored. Without Tailscale, give an address:
`syncthis peer add 100.70.66.9:22000`.

### 3. Add a folder

The same id on both devices. The path may differ.

```console
$ syncthis --data-dir /tmp/sdemo/a/data folder add /tmp/sdemo/a/docs --id docs
added folder docs at /tmp/sdemo/a/docs
$ syncthis --data-dir /tmp/sdemo/b/data folder add /tmp/sdemo/b/docs --id docs
added folder docs at /tmp/sdemo/b/docs
```

`folder add`, `folder set`, `folder remove`, `peer add`, `peer remove` and
`config set` all ask the running daemon to reload. Nothing is restarted.

### 4. Confirm convergence

Write a file on one side and read the other's report. One `jq` line covers it:

```console
$ echo "hello from A" > /tmp/sdemo/a/docs/greeting.txt
$ syncthis --data-dir /tmp/sdemo/b/data status --json | jq -c '{running: .daemon.running, peers: [.peers[] | {name, connected}], folders: [.folders[] | {id, state, files, bytes}], transfers: .transfers.active, conflicts: .conflicts.unresolved}'
{"running":true,"peers":[{"name":"laptop","connected":true}],"folders":[{"id":"docs","state":"idle","files":1,"bytes":13}],"transfers":0,"conflicts":0}
$ cat /tmp/sdemo/b/docs/greeting.txt
hello from A
```

The file arrives on the next sync cycle, 30 seconds by default and usually
sooner; the run above was read ten seconds after the write.

Converged means all four at once: every peer `connected: true`, every folder
`state: "idle"` with `pending_changes: 0` and `error: null`,
`transfers.active: 0`, and `conflicts.unresolved: 0`. A folder in `scheduled`
is outside its sync window and is waiting on purpose. `sync-plan` is the other
way to say converged: it prints what a sync would do without doing it.

```console
$ syncthis --data-dir /tmp/sdemo/b/data sync-plan docs
peer laptop (PQXYNPE2LEFKUFHG2DQCSTKYZ5NZ4XDTTNNKEOCWFTMMYNO3PJSQ) at 127.0.0.1:22001
ACTION  PATH          REASON
none    greeting.txt  in-sync

folder is in sync; nothing would change
```

Stop the two daemons when you are done: `pkill -f 'syncthis --data-dir
/tmp/sdemo'`, or SIGTERM each pid, which is what they shut down cleanly on.

### 5. Read the exit code

Every command exits with a code from the table in
[CLI.md](CLI.md#exit-codes), so a script never has to parse output. The ones
that come up during an install:

| Code | Meaning during install |
|---|---|
| 3 | the data directory has no identity: run `syncthis init` |
| 6 | the daemon is needed and none runs, or a second daemon tried to take a data directory |
| 7 | the peer could not be dialled, refused the handshake, or presented another identity |
| 2 | bad flags, a path that is not a directory, or a listen address that is public without `--allow-public` |
| 11 | a `network` command found no running Tailscale client |

```console
$ mkdir -p /tmp/sdemo/empty
$ syncthis --data-dir /tmp/sdemo/empty status; echo "exit=$?"
syncthis: device identity not initialized: /tmp/sdemo/empty/identity.key missing
exit=3
```

With `--json` the same failure is one document on stdout:
`{"error": {"code": 3, "message": "..."}}`.
