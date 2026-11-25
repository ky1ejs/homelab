# The mac studio's diagnostics email: what takes its disk and how its memory
# held up, mailed to Kyle's own inbox for a scheduled agent to turn into
# homelab PRs (docs/mac-studio.md, "Diagnostics email"; ./ROUTINE.md).
# ./mac-diagnose does the work; this runs it.
#
# A root daemon, because the scan reads every user's folders and the mail
# settings are root's alone. It runs four times a day and sends only when a
# report is due: the weekly one, or one for low disk. The mail settings,
# /etc/homelab/diagnose-mail, are not in this repo or the Nix store: `sudo
# mac-diagnose login` writes them.
{ pkgs, ... }:

let
  macDiagnose = pkgs.runCommand "mac-diagnose" { } ''
    install -Dm755 ${./mac-diagnose} $out/bin/mac-diagnose
    substituteInPlace $out/bin/mac-diagnose \
      --replace-fail '#!/usr/bin/env python3' '#!${pkgs.python3}/bin/python3' \
      --replace-fail '"@cacert@"' '"${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt"'
  '';
in
{
  environment.systemPackages = [ macDiagnose ];

  launchd.daemons.mac-diagnose.serviceConfig = {
    ProgramArguments = [ "${macDiagnose}/bin/mac-diagnose" "mail" ];
    # Local time. The weekly report waits for the first run at or after
    # 9:00 on Monday (WEEKLY_HOUR); a low-disk report goes at the next run.
    StartCalendarInterval = map (h: { Hour = h; Minute = 17; }) [ 3 9 15 21 ];
    # A weekly scan walks the whole Data volume with du: minutes, not seconds.
    LowPriorityIO = true;
    Nice = 10;
    StandardOutPath = "/var/log/mac-diagnose.log";
    StandardErrorPath = "/var/log/mac-diagnose.log";
  };
}
