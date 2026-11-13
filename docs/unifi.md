# The UniFi network

The gateway's configuration lives in UniFi, not in this repo. This page is its
record: the networks, the firewall zones, the policies added by hand, and how
to change them through UniFi's local Network Integration API without breaking
the house. As with [`host.md`](host.md), correct it only against the gateway.
The state below was read back from the API on 2026-11-13.

| | |
|---|---|
| Gateway | `192.168.1.1`, UniFi Network 10.6.109. Its certificate is self-signed |
| API | `https://192.168.1.1/proxy/network/integration/v1/`, key in the `X-API-KEY` header |
| Key | 1Password, `op://Private/Agent UniFi Admin Key/password` |
| Site | `Default`, id `88f7af54-98f8-306a-a1c7-c9349722b1f6`. Every path below starts `sites/<id>/` |

## Networks and zones

| Network | VLAN | Subnet | Zone | Wi-Fi | mDNS forwarding |
|---|---|---|---|---|---|
| Trusted | 1 | `192.168.1.0/24` | Internal | Satti-private | On |
| Guest | 2 | `192.168.6.0/24` | Hotspot | Satti-Fi | Off |
| IoT | 3 | `192.168.3.0/24` | IoT | Satti-IoT-Enhanced | On |
| Mac Studio | 5 | `192.168.40.0/24` | Mac Studio | None: wired only | Off |

The IoT and Mac Studio zones were made by hand on 2026-11-12. Before that,
IoT sat in Internal with Trusted, so the NAS could start connections to the
laptop. A Media network (VLAN 4) shared the IoT zone briefly. It had no Wi-Fi
and no clients, so it was deleted.

Hosts that policies or runbooks name:

| Host | Address | Notes |
|---|---|---|
| The NAS | `192.168.3.2`, fixed | On IoT, so it starts nothing towards Trusted. Home Assistant runs here |
| The Mac Studio | `192.168.40.2`, fixed, `mac-studio.lan` | [`mac-studio.md`](mac-studio.md#the-network) has why its zone allows what it does |
| The Hue bridge | On IoT, wired | Moved from Trusted on 2026-11-13, so Home Assistant reaches it without an exception into Trusted |

## Policies added by hand

UniFi's defaults do the rest. A custom zone starts out blocking everything to
and from the other internal zones, *and within itself*: two networks in one
custom zone cannot reach each other. It is allowed to reach the internet and
the gateway. Each Allow below that has replies on gets a derived
*(Return)* policy, matching `ESTABLISHED` and `RELATED` traffic only.

| Name | From | To | Action |
|---|---|---|---|
| Internal to IoT | Internal | IoT | Allow all, with replies |
| Trusted to Mac Studio SSH+VNC | Trusted network | Mac Studio | Allow TCP 22, 5900, with replies |
| Trusted to Mac Studio Screen Sharing UDP | Trusted network | Mac Studio | Allow UDP 5900–5902, with replies. High Performance Screen Sharing needs it |
| NAS to Mac Studio SSH | `192.168.3.2` | Mac Studio | Allow TCP 22, with replies |
| Mac Studio DNS | Mac Studio | Gateway | Allow TCP and UDP 53 |
| Mac Studio DHCP | Mac Studio | Gateway | Allow UDP 68 to 67 |
| Block Mac Studio to Gateway | Mac Studio | Gateway | Block all. It sits after the two above |

## Changing it through the API

The API changes the network the house runs on, so every session follows the
same loop:

1. **Read first.** Save the networks, zones and policies to a local JSON file
   before any write. That file is the undo.
2. **Take field names from those objects,** not from memory or the docs. The
   schema moves between Network versions. When no existing object shows a
   shape, such as a port range, do not guess it: list the ports one by one.
3. **Show each write before sending it:** method, path, body, and what it
   changes. Send it only after a yes for that write.
4. **Keep the IDs** of everything created or changed, so each one can be
   deleted or put back.
5. **Read back after** the write, including the policies UniFi adds by itself.

Read the key inside the command that uses it, and pass the header on stdin,
so the key appears in no argument list, file or shell history:

```sh
KEY=$(op read "op://Private/Agent UniFi Admin Key/password") || exit 1
printf 'X-API-KEY: %s\n' "$KEY" |
  curl -sS -k -H @- -H 'Accept: application/json' \
    "https://192.168.1.1/proxy/network/integration/v1/sites/88f7af54-98f8-306a-a1c7-c9349722b1f6/firewall/zones"
unset KEY
```

`-k` is for this host's self-signed certificate. Leave it out of any command
that talks to anything else. Writes add `-X POST|PUT|DELETE`,
`-H 'Content-Type: application/json'` and `--data @body.json`.

The paths used so far: `info`, `sites`, then under the site `networks`,
`networks/<id>`, `firewall/zones`, `firewall/zones/<id>`, `firewall/policies`,
`firewall/policies/<id>`, `firewall/policies/ordering`, `clients`, `devices/<id>`
and `wifi/broadcasts`. List endpoints page at 25, so add `?limit=200`.

### What the API does

Each of these was seen on 10.6.109:

- **`PUT firewall/zones/<id>` with `networkIds` moves networks** out of
  whichever zone held them. Policies that named a moved network in their old
  zone get disabled, with the network removed from their filter.
- **A new zone brings 18 default policies with it,** and moving a network
  into it adds per-zone ones such as *Allow mDNS* to the gateway.
- **Policies you add sit ahead of UniFi's, in creation order.** Create an
  Allow before the Block it must beat. `firewall/policies/ordering` shows the
  order, and needs both `sourceFirewallZoneId` and `destinationFirewallZoneId`.
- **`allowReturnTraffic: true` is refused on a policy to the Gateway zone**
  (`cant-allow-return-traffic`). The default *Gateway → zone: Allow All*
  carries the replies instead.
- **Policies made in the app before the API existed can come back with
  `id: null`.** The API cannot change or delete those; the app can.
- **`DELETE networks/<id>` works,** once no policy names the network.

### What it cannot do

These go through the UniFi app:

| Change | Where in the app |
|---|---|
| A switch port's VLAN | UniFi Devices → the switch → Port Manager → the port: Native VLAN, and Tagged VLAN Management set to Block All |
| A fixed IP or a local DNS record | Client Devices → the client → Fixed IP Address, Local DNS Record |
| Blocking a client | Client Devices → the client → Block |
| mDNS for some services only | Settings → Networks → Multicast DNS → Custom |

The API shows a switch's ports as link state and speed only, with no VLAN, so
it cannot say which network a wired device is on. The client list says that
by address.

## Traps

- **Wi-Fi gets around a VLAN.** A wired device that has also saved a home
  network joins both. The Studio was on Trusted over Wi-Fi after its port had
  moved. An Apple Account's password sync brings a forgotten network back, so
  turn it off on any Mac that has to stay put.
- **A move between zones cuts the moved side's outbound connections.** When
  the NAS left Internal, Home Assistant lost the Hue bridge, which was still
  on Trusted. Before moving a device, list what it starts connections to.
- **Bonjour remembers old addresses for over an hour.** After a move, a Finder
  sidebar entry can point at an address the device no longer has. Connect
  by name or address until the cache expires.
