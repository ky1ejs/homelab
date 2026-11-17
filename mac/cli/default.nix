# `homelab` and `hl` on the mac studio: one command for what the other
# modules run (./homelab, docs/mac-studio.md, "The homelab command"). The
# NAS's CLI is bin/homelab; this is the mac studio's, under the same name, so
# `hl status` means the same thing on either host.
#
# The names, paths, slot list and drain minutes come from the rest of the
# config, filled into ./homelab's placeholders here, so a slot added in mac/ci
# shows in `hl status` and `hl drain` with no change to the script. A few
# thresholds and the root daemons' names are copied into ./homelab by hand;
# AGENTS.md lists them, and each source points back here.
{ config, lib, pkgs, ... }:

let
  admin = config.system.primaryUser;
  worker = "worker";

  # mac/ci's slots, by the labels it registers for draining:
  # dev.kylejs.ci-runner-<slot>.
  slots = lib.sort (a: b: lib.toInt a < lib.toInt b)
    (map (lib.removePrefix "dev.kylejs.ci-runner-")
      (lib.filter (lib.hasPrefix "dev.kylejs.ci-runner-")
        (lib.attrNames config.homelab.worker.drain.commands)));

  cli = pkgs.runCommand "homelab-cli" { } ''
    install -Dm755 ${./homelab} $out/bin/homelab
    substituteInPlace $out/bin/homelab \
      --replace-fail '@admin@' ${lib.escapeShellArg admin} \
      --replace-fail '@worker@' ${lib.escapeShellArg worker} \
      --replace-fail '@workerHome@' ${lib.escapeShellArg "/Users/${worker}"} \
      --replace-fail '@checkout@' ${lib.escapeShellArg "/Users/${admin}/homelab"} \
      --replace-fail '@dotfiles@' ${lib.escapeShellArg "/Users/${admin}/kylejs-macOS-bootstrap"} \
      --replace-fail '@slots@' ${lib.escapeShellArg (lib.concatStringsSep " " slots)} \
      --replace-fail '@drainMinutes@' ${toString config.homelab.worker.drain.minutes} \
      --replace-fail '@sw@' /run/current-system/sw/bin
    ln -s homelab $out/bin/hl
  '';
in
{
  assertions = [{
    assertion = slots != [ ];
    message = "mac/cli: found no CI runner slots in homelab.worker.drain.commands";
  }];

  environment.systemPackages = [ cli ];
}
