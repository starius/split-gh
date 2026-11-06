{
  description = "split-gh: GitHub for agents in Qubes, without giving them your credentials";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/674a2028661e550a97400c1cc992b9cca9a668a1";

  outputs = { self, nixpkgs }:
    let
      forAllSystems = nixpkgs.lib.genAttrs [ "x86_64-linux" "aarch64-linux" ];
    in
    {
      packages = forAllSystems (system: {
        default = nixpkgs.legacyPackages.${system}.buildGoModule {
          pname = "split-gh";
          version = self.shortRev or self.dirtyShortRev or "dev";
          src = self;
          # Only the standard library.
          vendorHash = null;
          env.CGO_ENABLED = 0;
          # Building runs no code from this repository; the tests would.
          # nix flake check runs them, in the build sandbox.
          doCheck = false;
          meta.mainProgram = "split-gh";
        };
      });

      # gh only for the tests: they check serve's allowlist against it.
      checks = forAllSystems (system: {
        default = self.packages.${system}.default.overrideAttrs (old: {
          doCheck = true;
          nativeCheckInputs = with nixpkgs.legacyPackages.${system}; [ git gh jq ];
        });
      });
    };
}
