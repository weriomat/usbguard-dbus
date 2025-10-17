{
  inputs = {
    utils.url = "github:numtide/flake-utils";
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs =
    {
      self,
      utils,
      nixpkgs,
      ...
    }:
    utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
      in
      {
        packages = utils.lib.flattenTree rec {
          default = usbguard-dbus;
          usbguard-dbus =
            with pkgs;
            buildGoModule {
              name = "usbguard-dbus";
              src = self;

              vendorHash = "sha256-7s1mUMU7i+oEB7x5rq9T2G5ws3WmM1NQb/6SWb+Cg3Y=";
            };
        };
        devShells.default = pkgs.mkShell {
          buildInputs = [
            pkgs.go
          ];
        };
      }
    );
}
