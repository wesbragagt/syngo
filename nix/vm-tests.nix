# NixOS VM tests for the module. These boot a machine and run the daemon, so
# they prove what an evaluation cannot: that the unit starts, that the data
# directory is the one the daemon can open, and that the documented first-run
# failure is the one an operator actually sees.
#
# They are checks, so `nix flake check` runs them on Linux.
{ self, pkgs }:

let
  # The module under test, plus the settings both tests share.
  base = { config, ... }: {
    imports = [ self.nixosModules.default ];
    services.syngo.enable = true;
    # A VM has little memory and the test drives the daemon by hand.
    virtualisation.memorySize = 1024;
  };

  dataDir = "/var/lib/syngo";

  # Every command the test runs on behalf of the daemon's user runs with the
  # same data directory the unit gives it, so the CLI talks to the running
  # daemon's control socket.
  asUser = cmd: "sudo -u syngo env SYNGO_DATA_DIR=${dataDir} ${cmd}";
in
{
  # One machine: the failure before `syngo init`, then the service running.
  module = pkgs.testers.runNixOSTest {
    name = "syngo-module";

    nodes.machine = base;

    testScript = ''
      machine.wait_for_unit("multi-user.target")

      # The data directory is created by systemd-tmpfiles before the daemon
      # needs it, and only its user may read it: it holds the private key.
      machine.succeed("test -d ${dataDir}")
      mode, owner = machine.succeed(
          "stat -c '%a %U:%G' ${dataDir}"
      ).strip().split()
      assert mode == "700", f"data directory mode is {mode}, want 700"
      assert owner == "syngo:syngo", f"data directory owner is {owner}"

      # Documented first-run behaviour: with no identity the daemon exits 3
      # and says which file is missing. Restart=always then brings it back,
      # so the unit is in a restart loop rather than a failed state, and that
      # is on purpose: it starts on its own once the identity exists.
      machine.wait_until_succeeds(
          "test \"$(systemctl show syngo.service -p ExecMainStatus --value)\" = 3"
      )
      machine.wait_until_succeeds(
          "journalctl -u syngo.service | grep -q 'device identity not initialized'"
      )
      machine.succeed(
          "journalctl -u syngo.service | grep -q '${dataDir}/identity.key missing'"
      )

      # Give it an identity, the one thing the module cannot do for the
      # operator, and the unit comes up.
      machine.succeed(
          "${asUser "syngo init --name machine --listen 127.0.0.1:22000"}"
      )
      machine.wait_for_unit("syngo.service")
      machine.wait_until_succeeds(
          "test \"$(systemctl show syngo.service -p ActiveState --value)\" = active"
      )

      # The daemon answers as its own user, over the control socket in the
      # data directory, and reports itself running. `status` succeeds with no
      # daemon too, so the running flag is what is waited on.
      import json

      def running(machine):
          out = machine.succeed("${asUser "syngo status --json"}")
          return json.loads(out)["daemon"]["running"]

      retry(lambda _: running(machine))

      # SIGTERM is the stop signal the unit sets, and `daemon run` shuts down
      # cleanly on it: systemd sees a clean stop, not a timeout and a kill.
      machine.succeed("systemctl stop syngo.service")
      assert not running(machine), "daemon still reports itself running after stop"
    '';
  };

  # Two machines on one network: the module on both, paired over the test
  # network interface, and a file that has to arrive on the other side. This
  # is what the module is for, so it is worth two VMs.
  pair = pkgs.testers.runNixOSTest {
    name = "syngo-module-pair";

    nodes = {
      # eth1 is the interface the test driver puts both machines on. Its
      # address is RFC 1918, so the daemon binds it without --allow-public,
      # the same way it binds tailscale0 on a tailnet.
      # The port is opened here, not by the module: which port a daemon
      # listens on comes from its own config.json, so opening one is the
      # operator's decision, not the module's.
      a = { imports = [ base ]; services.syngo.interface = "eth1"; networking.firewall.allowedTCPPorts = [ 22000 ]; };
      b = { imports = [ base ]; services.syngo.interface = "eth1"; networking.firewall.allowedTCPPorts = [ 22000 ]; };
    };

    testScript = ''
      start_all()

      for machine, name in ((a, "a"), (b, "b")):
          machine.wait_for_unit("multi-user.target")
          machine.succeed(
              f"${asUser "syngo init --name {name} --listen 0.0.0.0:22000"}"
          )
          machine.succeed("mkdir -p /srv/docs && chown syngo:syngo /srv/docs")
          machine.succeed(
              "${asUser "syngo folder add /srv/docs --id docs"}"
          )
          machine.succeed("systemctl restart syngo.service")
          machine.wait_for_unit("syngo.service")
          machine.wait_until_succeeds(
              "${asUser "syngo status --json"} | grep -q '\"running\": true'"
          )

      # Each side trusts the other by the address the test network gave it.
      a.succeed("${asUser "syngo peer add 192.168.1.2:22000 --yes"}")
      b.succeed("${asUser "syngo peer add 192.168.1.1:22000 --yes"}")

      a.succeed("echo 'hello from a' > /srv/docs/greeting.txt")
      a.succeed("chown syngo:syngo /srv/docs/greeting.txt")

      b.wait_until_succeeds(
          "grep -q 'hello from a' /srv/docs/greeting.txt", timeout=180
      )

      # And the other way, so neither direction is an accident.
      b.succeed("echo 'hello from b' > /srv/docs/reply.txt")
      b.succeed("chown syngo:syngo /srv/docs/reply.txt")
      a.wait_until_succeeds(
          "grep -q 'hello from b' /srv/docs/reply.txt", timeout=180
      )
    '';
  };
}
