{
  description = "Anti-API Gateway for Google Antigravity";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs =
    { self, nixpkgs }:
    let
      supportedSystems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      forAllSystems = nixpkgs.lib.genAttrs supportedSystems;
      pkgsFor = system: import nixpkgs { inherit system; };
    in
    {
      packages = forAllSystems (
        system:
        let
          pkgs = pkgsFor system;
        in
        {
          default = pkgs.buildGoModule {
            pname = "antiapi";
            version = "1.1.0";
            src = ./.;
            vendorHash = null;
            subPackages = [ "." ];
            env = {
              CGO_ENABLED = "0";
            };
            nativeBuildInputs = [ pkgs.makeWrapper ];
            postInstall = ''
              wrapProgram $out/bin/antiapi \
                --prefix PATH : ${pkgs.lib.makeBinPath [ pkgs.cloudflared ]}
            '';
          };
        }
      );

      apps = forAllSystems (
        system:
        let
          pkgs = pkgsFor system;
        in
        {
          default = {
            type = "app";
            program = "${self.packages.${system}.default}/bin/antiapi";
          };
          tunnel = {
            type = "app";
            program = "${
              pkgs.writeShellScriptBin "antiapi-tunnel" ''
                exec ${self.packages.${system}.default}/bin/antiapi --tunnel "$@"
              ''
            }/bin/antiapi-tunnel";
          };
        }
      );

      devShells = forAllSystems (
        system:
        let
          pkgs = pkgsFor system;
        in
        {
          default = pkgs.mkShell {
            packages = with pkgs; [
              go
              gopls
              gotools
              golangci-lint
              git
              cacert
              python3
              cloudflared
            ];

            shellHook = ''
              export CGO_ENABLED=0
            '';
          };
        }
      );
    };
}
