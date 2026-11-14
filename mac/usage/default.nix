# What the Mac Studio does, and what it would have cost on hosted Macs: the
# data for deciding whether it pays its way (docs/mac-studio.md, "Usage and
# cost"). ./mac-usage does the work; this runs it.
#
# - A root daemon samples the host every minute: load, CPU, memory, VMs,
#   Claude and Xcode processes, chip power and thermal pressure. Root because
#   powermetrics needs it. It runs before anyone logs in, so a Mac Studio
#   waiting for worker still records.
# - A worker agent collects every 15 minutes: Cut's CI jobs from GitHub, with
#   worker's cut-watch token (Actions: read), and the agent sessions in
#   worker's Claude Code transcripts. It runs only while worker is logged in,
#   and catches up from its last run when worker logs in again.
# - `mac-usage report` prints the summary, for anyone.
{ config, lib, pkgs, ... }:

let
  macUsage = pkgs.runCommand "mac-usage" { } ''
    install -Dm755 ${./mac-usage} $out/bin/mac-usage
    substituteInPlace $out/bin/mac-usage \
      --replace-fail '#!/usr/bin/env python3' '#!${pkgs.python3}/bin/python3'
  '';
in
{
  environment.systemPackages = [ macUsage ];

  launchd.daemons.mac-usage.serviceConfig = {
    ProgramArguments = [ "${macUsage}/bin/mac-usage" "sample" ];
    RunAtLoad = true;
    StartInterval = 60;
    # A sample takes about 3 seconds: top and powermetrics each wait one.
    LowPriorityIO = true;
    Nice = 10;
    StandardOutPath = "/var/log/mac-usage.log";
    StandardErrorPath = "/var/log/mac-usage.log";
  };

  homelab.worker.agents = [{
    Label = "dev.kylejs.mac-usage";
    ProgramArguments = [ "${macUsage}/bin/mac-usage" "collect" ];
    RunAtLoad = true;
    StartInterval = 15 * 60;
    LowPriorityIO = true;
    Nice = 10;
    StandardOutPath = "/Users/worker/Library/Logs/mac-usage.log";
    StandardErrorPath = "/Users/worker/Library/Logs/mac-usage.log";
    LimitLoadToSessionType = "Aqua";
  }];
}
