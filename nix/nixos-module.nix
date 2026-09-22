# Runs the syncthis daemon as a system-wide systemd service for one named
# user. The unit is the one packaging/systemd/syncthis.service describes; the
# only additions are the ones a system service needs and a user service does
# not: User, Group and the state directory.
#
# The daemon needs an identity before it starts. Run it once as that user:
#   sudo -u syncthis env SYNCTHIS_DATA_DIR=/var/lib/syncthis syncthis init --name <name>
# Until then the unit exits and systemd keeps restarting it, which is the
# documented restart policy, not a bug.
{ self }:
{ config, lib, pkgs, ... }:

let
  service = import ./service.nix { inherit lib; };
  cfg = config.services.syncthis;
in
{
  options.services.syncthis = service.mkOptions {
    defaultPackage = self.packages.${pkgs.stdenv.hostPlatform.system}.syncthis;
    defaultDataDir = "/var/lib/syncthis";
  } // {
    user = lib.mkOption {
      type = lib.types.str;
      default = "syncthis";
      description = ''
        The user the daemon runs as. It owns the data directory and every file
        the daemon writes, so it needs read and write access to each
        synchronized folder.
      '';
    };

    group = lib.mkOption {
      type = lib.types.str;
      default = "syncthis";
      description = "The group the daemon runs as.";
    };

    createUser = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = ''
        Create the user and group above. Set to false to run the daemon as a
        user this configuration already declares elsewhere.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    users.users = lib.mkIf cfg.createUser {
      ${cfg.user} = {
        isSystemUser = true;
        group = cfg.group;
        home = cfg.dataDir;
        description = "syncthis daemon";
      };
    };
    users.groups = lib.mkIf cfg.createUser { ${cfg.group} = { }; };

    environment.systemPackages = [ cfg.package ];

    # The data directory must exist and belong to the daemon's user before it
    # starts. StateDirectory= would only do this for a path under /var/lib, and
    # dataDir may be anywhere.
    systemd.tmpfiles.rules = [
      "d ${cfg.dataDir} 0700 ${cfg.user} ${cfg.group} - -"
    ];

    systemd.services.syncthis = {
      description = service.unitSection.Description;
      documentation = [ service.unitSection.Documentation ];
      after = service.unitSection.After;
      wants = service.unitSection.Wants;
      wantedBy = [ "multi-user.target" ];
      unitConfig.StartLimitIntervalSec = service.unitSection.StartLimitIntervalSec;
      serviceConfig = service.serviceSection cfg // {
        User = cfg.user;
        Group = cfg.group;
      };
    };
  };
}
