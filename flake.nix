{
  description = "toll — a minimal, self-hosted LLM gateway for OpenAI-compatible APIs";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-parts.url = "github:hercules-ci/flake-parts";
    flake-parts.inputs.nixpkgs-lib.follows = "nixpkgs";
    import-tree.url = "github:denful/import-tree";
  };

  outputs =
    inputs:
    inputs.flake-parts.lib.mkFlake { inherit inputs; } {
      # Linux only: the gateway ships as a static binary and a Nix-built OCI image.
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];

      # Dendritic pattern: every module under nix/flake/ is imported automatically,
      # so flake outputs are added by dropping in a file rather than editing this one.
      imports = [ (inputs.import-tree ./nix) ];
    };
}
