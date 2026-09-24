{
  description = "syngo, folder synchronization between your own devices";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    let
      # The stamp `syngo version` reports. The Makefile takes it from
      # `git describe --tags --always --dirty` and `git rev-parse --short`; a
      # flake build has no git, so it uses what the flake itself knows about
      # the checkout. A build from a dirty tree is marked as one, the same way
      # git describe --dirty marks it.
      # dirtyShortRev already carries the -dirty suffix git describe adds, so
      # version takes it as it is and commit strips it back to the bare
      # revision `git rev-parse --short` prints.
      version = self.shortRev or self.dirtyShortRev or "dev";
      commit = nixpkgs.lib.removeSuffix "-dirty" version;
    in
    # The systems syngo is built for. x86_64-darwin is left out because
    # this nixpkgs has dropped it; the Linux three are the `make dist`
    # targets that Nix can build.
    flake-utils.lib.eachSystem [ "x86_64-linux" "aarch64-linux" "aarch64-darwin" ]
      (system:
        let
          pkgs = import nixpkgs { inherit system; };
          syngo = pkgs.callPackage ./nix/package.nix { inherit version commit; };
          vmTests = import ./nix/vm-tests.nix { inherit self pkgs; };
        in
        {
          packages = {
            inherit syngo;
            default = syngo;
          };

          apps.default = {
            type = "app";
            program = "${pkgs.lib.getExe syngo}";
          };

          devShells.default = pkgs.mkShell {
            buildInputs = with pkgs; [
              go
              gopls
              golangci-lint
              git
            ];
          };

          checks = {
            package = syngo;
          } // pkgs.lib.optionalAttrs pkgs.stdenv.hostPlatform.isLinux {
            # The NixOS module has to evaluate, not just parse: a bad option
            # type or a reference to a package that does not exist on this
            # system only shows up when a configuration using it is built.
            nixos-module = (nixpkgs.lib.nixosSystem {
              inherit system;
              modules = [
                self.nixosModules.default
                ({ lib, ... }: {
                  boot.loader.grub.devices = [ "nodev" ];
                  fileSystems."/" = { device = "none"; fsType = "tmpfs"; };
                  system.stateVersion = lib.versions.majorMinor lib.version;
                  services.syngo = {
                    enable = true;
                    interface = "tailscale0";
                  };
                })
              ];
            }).config.system.build.toplevel;

            # Booting the module is the only thing that proves the daemon
            # runs: the toplevel above builds a system that was never started.
            vm-module = vmTests.module;
            vm-module-pair = vmTests.pair;
          };
        })
    // {
      nixosModules.syngo = import ./nix/nixos-module.nix { inherit self; };
      nixosModules.default = self.nixosModules.syngo;

      homeManagerModules.syngo = import ./nix/hm-module.nix { inherit self; };
      homeManagerModules.default = self.homeManagerModules.syngo;
    };
}
