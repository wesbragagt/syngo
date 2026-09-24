# Runs the same daemon as a systemd user service, which is what
# packaging/systemd/syngo.service is. It is a plain module function, so it
# needs no flake input: home-manager evaluates it with its own lib and pkgs.
#
# Not tested. `nix flake check` does not evaluate this module: homeManagerModules
# is not an output nix knows, so it only warns about the name and moves on, and
# there is no home-manager input here to evaluate it against by design. It has
# been evaluated only by hand, against stub options standing in for
# config.xdg.dataHome, home.packages and systemd.user.services, which proves
# the module's own expressions but not that home-manager accepts them. The
# NixOS module, which shares nix/service.nix with this one, is covered by a VM
# test; this one is not. Read it as untested until someone runs it under real
# home-manager.
#
# The daemon needs an identity before it starts: run `syngo init` once.
# Add `loginctl enable-linger $USER` to keep it running when that user is not
# logged in.
{ self }:
{ config, lib, pkgs, ... }:

let
  service = import ./service.nix { inherit lib; };
  cfg = config.services.syngo;
in
{
  options.services.syngo = service.mkOptions {
    defaultPackage = self.packages.${pkgs.stdenv.hostPlatform.system}.syngo;
    defaultDataDir = "${config.xdg.dataHome}/syngo";
  };

  config = lib.mkIf cfg.enable {
    home.packages = [ cfg.package ];

    systemd.user.services.syngo = {
      Unit = service.unitSection;
      Service = service.serviceSection cfg;
      Install.WantedBy = [ "default.target" ];
    };
  };
}
