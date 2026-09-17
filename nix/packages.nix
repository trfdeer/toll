{ self, ... }:
{
  perSystem =
    {
      pkgs,
      config,
      ...
    }:
    let
      version = if self ? rev then "0.1.0-${self.shortRev}" else "0.1.0-dev";
      hashes = builtins.fromJSON (builtins.readFile ./hashes.json);
    in
    {
      packages.toll = pkgs.buildGoModule {
        pname = "toll";
        inherit version;
        src = ../.;

        # Embed the freshly built Vite bundle; admin.go does
        # //go:embed all:web/dist, so the directory must exist before `go build`.
        preBuild = ''
          rm -rf internal/admin/web/dist
          mkdir -p internal/admin/web/dist
          cp -R ${config.packages.web}/. internal/admin/web/dist/
        '';

        # platform-independent; updated via: nix build 2>&1 | grep got:
        vendorHash = hashes.goVendor;
        ldflags = [
          "-s"
          "-w"
          "-X main.version=${version}"
        ];
        meta = {
          description = "A minimal, self-hosted LLM gateway for OpenAI-compatible APIs";
          mainProgram = "toll";
        };
      };

      packages.default = config.packages.toll;

      # OCI/Docker image, built by Nix itself (no Docker daemon needed):
      #   nix build .#image && docker load < result
      # Distroless-style static binary, nonroot, /data volume, built-in healthcheck.
      packages.image = pkgs.dockerTools.buildLayeredImage {
        name = "toll";
        tag = version;
        contents = with pkgs; [
          cacert # TLS for HTTPS upstreams
          fakeNss # /etc/passwd + group for the nonroot user
        ];
        config = {
          Entrypoint = [ "${config.packages.toll}/bin/toll" ];
          Env = [
            "TOLL_LISTEN=:8080"
            "TOLL_DATA_DIR=/data"
            "SSL_CERT_FILE=${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt"
          ];
          ExposedPorts = {
            "8080/tcp" = { };
          };
          User = "65534:65534"; # nobody:nogroup
          Volumes = {
            "/data" = { };
          };
          WorkingDir = "/data";
          Healthcheck = {
            Test = [
              "CMD"
              "${config.packages.toll}/bin/toll"
              "healthcheck"
              "--url=http://127.0.0.1:8080/healthz"
            ];
            Interval = 30000000000; # 30s in ns
            Timeout = 3000000000; # 3s
            StartPeriod = 10000000000;
            Retries = 3;
          };
        };
      };

      apps.default = {
        type = "app";
        program = "${config.packages.toll}/bin/toll";
      };
    };
}
