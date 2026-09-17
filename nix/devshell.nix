{
  perSystem =
    { pkgs, ... }:
    {
      formatter = pkgs.nixfmt-tree;

      devShells.default = pkgs.mkShell {
        buildInputs = with pkgs; [
          go
          bun
          nodejs
        ];

        packages = with pkgs; [
          nixd
          nixfmt

          gopls
          gotools
          go-tools
          govulncheck
        ];
      };
    };
}
