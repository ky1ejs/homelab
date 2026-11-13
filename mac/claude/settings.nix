# agent's ~/.claude/settings.json, the settings every Remote Control session on
# the Studio starts from. mac/claude/default.nix writes it on each apply.
#
# The Bash sandbox is on for every server but Cut's: Cut builds and tests iOS,
# and the sandbox blocks CoreSimulator, so Cut's .claude/settings.local.json
# turns it off. Project local settings outrank user settings.
{
  sandbox = {
    enabled = true;
    # No sandbox, no session: never fall back to running commands bare.
    failIfUnavailable = true;
    # A blocked command stays blocked; Claude cannot retry it outside.
    allowUnsandboxedCommands = false;
    # Sandboxed commands reach only these hosts. A session in auto mode is
    # refused any other host, so add one here when a repo needs it.
    network.allowedDomains = [
      "github.com"
      "api.github.com"
      "*.githubusercontent.com"
      "registry.npmjs.org"
      "cache.nixos.org"
      "channels.nixos.org"
    ];
    # The heartbeat's ping URL lives in ~/.config/claude-rc. agent holds no
    # SSH key, and the sandbox breaks git over SSH anyway, so ~/.ssh is shut
    # too.
    credentials.files = [
      { path = "~/.ssh"; mode = "deny"; }
      { path = "~/.config/claude-rc"; mode = "deny"; }
    ];
  };
}
