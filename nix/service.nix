# The one place that says what the syngo daemon unit is. The NixOS module
# and the home-manager module both build their unit from here, so the restart
# policy, the stop signal and the hardening cannot drift apart.
#
# It encodes exactly what packaging/systemd/syngo.service already encoded.
{ lib }:

rec {
  # Options every module that runs the daemon shares. defaultDataDir differs:
  # a system service keeps state under /var/lib, a user service under
  # $XDG_DATA_HOME.
  mkOptions = { defaultPackage, defaultDataDir }: {
    enable = lib.mkEnableOption "the syngo file synchronization daemon";

    package = lib.mkOption {
      type = lib.types.package;
      default = defaultPackage;
      description = "The syngo package to run.";
    };

    dataDir = lib.mkOption {
      type = lib.types.str;
      default = defaultDataDir;
      description = ''
        The directory the daemon opens: identity, config.json, state.db and
        the control socket. Passed as SYNGO_DATA_DIR.
      '';
    };

    interface = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = "tailscale0";
      description = ''
        Network interface whose first IPv4 address the daemon binds, passed as
        `daemon run --interface`. null leaves the listen address from
        config.json alone.
      '';
    };

    restart = lib.mkOption {
      type = lib.types.enum [ "always" "on-failure" "no" ];
      default = "always";
      description = ''
        systemd Restart= for the daemon. The default brings it back after any
        exit, including a peer or filesystem failure it could not recover from
        itself; the restart rate limit is disabled so a burst of restarts
        cannot leave the unit failed.
      '';
    };

    restartSec = lib.mkOption {
      type = lib.types.int;
      default = 5;
      description = "Seconds systemd waits before restarting the daemon.";
    };
  };

  execStart = cfg:
    "${lib.getExe cfg.package} daemon run"
    + lib.optionalString (cfg.interface != null) " --interface ${cfg.interface}";

  # The [Unit] section. StartLimitIntervalSec=0 belongs here, not in
  # [Service]: it is the section systemd reads it from, and without it a
  # crash-looping daemon stops coming back.
  unitSection = {
    Description = "syngo file synchronization daemon";
    Documentation = "https://github.com/wesbragagt/syngo";
    After = [ "network-online.target" ];
    Wants = [ "network-online.target" ];
    StartLimitIntervalSec = 0;
  };

  # The [Service] section, minus anything only a system service can set.
  serviceSection = cfg: {
    Environment = [ "SYNGO_DATA_DIR=${cfg.dataDir}" ];
    ExecStart = execStart cfg;
    Restart = cfg.restart;
    RestartSec = cfg.restartSec;
    # SIGTERM is what `daemon run` shuts down cleanly on: it finishes the
    # chunk in flight, releases the control socket and removes the pid file.
    KillSignal = "SIGTERM";
    TimeoutStopSec = 30;
    NoNewPrivileges = true;
    PrivateTmp = true;
    ProtectSystem = "full";
  };
}
