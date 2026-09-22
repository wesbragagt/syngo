# Runs the same daemon as a systemd user service, which is what
# packaging/systemd/syncthis.service is. It is a plain module function, so it
# needs no flake input: home-manager evaluates it with its own lib and pkgs.
#
# The daemon needs an identity before it starts: run `syncthis init` once.
# Add `loginctl enable-linger $USER` to keep it running when that user is not
# logged in.
{ self }:
{ config, lib, pkgs, ... }:

let
  service = import ./service.nix { inherit lib; };
  cfg = config.services.syncthis;
in
{
  options.services.syncthis = service.mkOptions {
    defaultPackage = self.packages.${pkgs.stdenv.hostPlatform.system}.syncthis;
    defaultDataDir = "${config.xdg.dataHome}/syncthis";
  };

  config = lib.mkIf cfg.enable {
    home.packages = [ cfg.package ];

    systemd.user.services.syncthis = {
      Unit = service.unitSection;
      Service = service.serviceSection cfg;
      Install.WantedBy = [ "default.target" ];
    };
  };
}
