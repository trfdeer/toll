{ self, ... }:
let
  rev = self.shortRev or self.dirtyShortRev or "dirty";
  hashes = builtins.fromJSON (builtins.readFile ./hashes.json);
in
{
  perSystem =
    {
      pkgs,
      lib,
      system,
      ...
    }:
    let
      version = if self ? rev then "0.1.0-${self.shortRev}" else "0.1.0-dev";

      mkNodeModules =
        hash:
        pkgs.stdenvNoCC.mkDerivation {
          pname = "toll-web-node_modules";
          version = "0.1.0+${lib.replaceString "-" "." rev}";

          src = lib.fileset.toSource {
            root = ../.;
            fileset = lib.fileset.unions [
              ../web/package.json
              ../web/bun.lock
            ];
          };

          impureEnvVars = lib.fetchers.proxyImpureEnvVars;
          nativeBuildInputs = [ pkgs.bun ];
          dontConfigure = true;

          buildPhase = ''
            runHook preBuild
            export BUN_INSTALL_CACHE_DIR=$(mktemp -d)
            (cd web && bun install --frozen-lockfile --ignore-scripts --no-progress)
            runHook postBuild
          '';

          installPhase = ''
            runHook preInstall
            mkdir -p $out/web
            cp -R web/node_modules $out/web/node_modules
            runHook postInstall
          '';

          dontFixup = true;

          outputHashAlgo = "sha256";
          outputHashMode = "recursive";
          outputHash = hash;

          meta.platforms = [
            "aarch64-linux"
            "x86_64-linux"
          ];
        };

      webNodeModules = mkNodeModules (hashes.webNodeModules.${system} or lib.fakeHash);
    in
    {
      packages.web-node-modules = webNodeModules;

      packages.web = pkgs.stdenvNoCC.mkDerivation {
        pname = "toll-web";
        inherit version;

        src = lib.fileset.toSource {
          root = ../.;
          fileset = lib.fileset.unions [
            ../web/index.html
            ../web/package.json
            ../web/tsconfig.json
            ../web/vite.config.ts
            ../web/src
          ];
        };

        nativeBuildInputs = [
          pkgs.bun
          pkgs.nodejs
          pkgs.writableTmpDirAsHomeHook
        ];

        configurePhase = ''
          runHook preConfigure
          mkdir -p web
          cp -R ${webNodeModules}/web/node_modules web/node_modules
          patchShebangs web/node_modules
          runHook postConfigure
        '';

        buildPhase = ''
          runHook preBuild
          mkdir -p internal/admin/web/dist
          (cd web && bun run build)
          runHook postBuild
        '';

        installPhase = ''
          runHook preInstall
          mkdir -p $out
          cp -R internal/admin/web/dist/. $out/
          runHook postInstall
        '';

        meta = {
          description = "toll admin web UI (Vite bundle embedded by the Go binary)";
          inherit (webNodeModules.meta) platforms;
        };
      };

      # Building this fails with a hash mismatch and prints the hash to record in
      # nix/hashes.json:  nix build .#web-node-modules-updater 2>&1 | grep 'got:'
      packages.web-node-modules-updater = mkNodeModules lib.fakeHash;
    };
}
