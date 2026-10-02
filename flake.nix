{
  description = "lazymcp: start a stdio MCP server only when a client first uses it";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      forAll = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
      version = "0.1.0";
    in
    {
      packages = forAll (pkgs: {
        default = pkgs.buildGoModule {
          pname = "lazymcp";
          inherit version;
          src = self;
          # Standard library only — nothing to vendor.
          vendorHash = null;
          subPackages = [ "cmd/lazymcp" ];
          env.CGO_ENABLED = "0";
          ldflags = [
            "-s"
            "-w"
            "-X main.version=${version}"
          ];
          meta = {
            description = "Start a stdio MCP server only when a client first uses it";
            mainProgram = "lazymcp";
            license = pkgs.lib.licenses.mit;
          };
        };
      });

      devShells = forAll (pkgs: {
        default = pkgs.mkShell {
          packages = [
            pkgs.go
            pkgs.gopls
          ];
        };
      });
    };
}
