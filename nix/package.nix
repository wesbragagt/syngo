# The syncthis binary, built the way `make release` builds it: CGO_ENABLED=0,
# -trimpath, and the version and commit stamped into internal/cli.
#
# Windows is out of scope, so there is nothing here about it.
{ lib
, buildGoModule
, version ? "dev"
, commit ? "unknown"
}:

buildGoModule {
  pname = "syncthis";
  inherit version;

  src = lib.cleanSource ../.;

  vendorHash = "sha256-oEqT0lsii+rfTaNpmCUa293A1OePJ/8LVqqwFflnFRw=";

  # Every dependency is pure Go. Without cgo the binary carries no dynamic
  # interpreter, which is what makes a copy of it runnable off the Nix store.
  env.CGO_ENABLED = 0;

  subPackages = [ "cmd/syncthis" ];

  ldflags = [
    "-X github.com/wesbragagt/syncthis/internal/cli.Version=${version}"
    "-X github.com/wesbragagt/syncthis/internal/cli.Commit=${commit}"
  ];

  # The end-to-end tests start two daemons on loopback and are run by
  # `make test`, not by the packaging.
  doCheck = false;

  meta = {
    description = "Folder synchronization between your own devices over a tailnet";
    homepage = "https://github.com/wesbragagt/syncthis";
    mainProgram = "syncthis";
    platforms = lib.platforms.linux ++ lib.platforms.darwin;
  };
}
