# Fixed facts about the ad hoc builds server, shared by mac/builds and
# mac/app-store's `adhoc`: the folder `adhoc` writes is the one the server
# serves, at the address the server's tailnet node takes.
#
# Both folders are under worker's ~/code because Colima's VM mounts that and
# nothing else (mac/colima), so a container can bind-mount only files there.
rec {
  user = "worker";
  # The builds, one random folder each. Served read-only.
  dir = "/Users/${user}/code/.builds";
  # The web server's and the sidecar's config, copied here from the Nix store
  # at each start, because the VM cannot see the store.
  confDir = "/Users/${user}/code/.builds-config";
  # The sidecar's tailnet node name and tag. The node name is the address's
  # first label: a second node with the same name would be builds-1, and
  # `builds login` says so.
  node = "builds";
  tag = "tag:builds";
  # This tailnet's MagicDNS suffix, already in the repo (docs/mac-studio.md).
  tailnet = "tail3df177.ts.net";
  url = "https://${node}.${tailnet}";
  # The sweep deletes a build this many days after it was made.
  keepDays = 7;
}
