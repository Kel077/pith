<div align="center">

<img src="resources/icon.png" width="112" alt="Pith">

# Pith

### Choose the network each app goes out through.

**WeChat over your phone's hotspot. The office intranet over Ethernet. Everything else as usual.**<br>
Write a few rules by app, domain or IP, and new connections leave through the interface you pick.<br>
You can also see which interface every connection really used.

[![License](https://img.shields.io/badge/license-MIT-111111)](LICENSE) [![Stars](https://img.shields.io/github/stars/Kel077/pith?style=flat&color=111111)](https://github.com/Kel077/pith/stargazers)<br>
![macOS 13+](https://img.shields.io/badge/macOS_13+-000000?logo=apple&logoColor=white) ![Apple silicon](https://img.shields.io/badge/Apple_silicon-000000?logo=apple&logoColor=white) ![Go](https://img.shields.io/badge/Go-00ADD8?logo=go&logoColor=white)

**[Architecture](docs/ARCHITECTURE.md)** &nbsp;·&nbsp; **[Design](docs/DESIGN.md)** &nbsp;·&nbsp; **English** · [简体中文](README.zh-CN.md)

<br>

<img src="docs/images/en/rules.png" width="900" alt="Pith's Rules page: WeChat goes out through an iPhone, then Ethernet; a NAS through Ethernet; ads are blocked">

</div>

<br>

## Is Pith for you?

A Mac can be on Ethernet, Wi-Fi and a phone's hotspot at the same time, but macOS sends almost everything through just one of them. Pith lets you decide, app by app and destination by destination:

- **Give one app its own line**: a chat app over the hotspot, a download tool over the second connection, everything else as before.
- **Keep the intranet on the wire**: company hosts or a NAS over Ethernet, the rest over Wi-Fi.
- **Switch lines when one fails**: a rule tries its interfaces in order, moving to the next when one can't connect; if none can, it hands the connection back to macOS, or drops it if you'd rather keep the rule strict.
- **See where everything went**: each connection with its app, its destination, the rule that matched and the interface it actually used.

If your Mac is often on more than one network and you want certain apps to stick to one of them, Pith is for you. If you only ever use one network, you won't need it.

Runs on Macs with Apple silicon and macOS 13 or later.

## Quick start

**1 · Build.** Pith isn't released yet. With [Go](https://go.dev/dl/) 1.27 or later:

```sh
git clone https://github.com/Kel077/pith.git
cd pith
go run ./cmd/pith          # the app, in the menu bar
go run ./cmd/pith -mock    # the interface on simulated data, nothing installed
```

<sub>The first time you turn routing on, macOS asks for an administrator's password to install Pith's helper, which does the routing. Settings → Helper removes it at any time.</sub>

**2 · Add a rule.** On **Rules**, choose **Add Rule**: the apps, the destinations (domains, IP addresses or ranges), and the interfaces to try, in order.

**3 · Turn routing on** from the menu bar or the sidebar. New connections follow the rules at once, and **Activity** shows where each one went.

## How it fits together

```mermaid
flowchart LR
  apps["Apps"] -- new connections --> utun["utun<br/>(captured while routing is on)"]
  utun --> pithd["pithd<br/>helper, runs as root"]
  pithd -- "bound to the chosen interface" --> eth["Ethernet"]
  pithd --> wifi["Wi-Fi"]
  pithd --> phone["iPhone, hotspot"]
  app["Pith<br/>menu bar and window"] <-- "Unix socket" --> pithd
```

While routing is on, the helper captures new connections through a utun interface and a user-space network stack, finds the app each belongs to from the kernel's socket tables, and the domain from TLS SNI, the HTTP Host or DNS. It then makes the connection again, bound to the interface the rules choose, and relays the bytes. Turning routing off removes everything it added to the routing table. Pith needs no Apple developer account or system extension: macOS asks once for the password that installs the helper.

## What it does

### Rules as sentences

Each rule reads like a sentence: these apps, going to these places, leave through these interfaces, in this order.

| | |
| :-- | :-- |
| **Apps** | Any app, or the apps you pick. An app's helpers, and the system services that connect for it (WebKit, nsurlsessiond), match too |
| **Destinations** | Domains (with or without their subdomains), IP addresses and CIDR ranges, IPv4 and IPv6; paste a list to add many |
| **Interfaces** | An ordered list: the next one is tried when one can't connect, and then macOS's routing if you allow it |
| **Block** | Block rules always win; otherwise the first matching rule from the top decides. Drag to reorder |
| **Checks** | A rule that an earlier one fully covers says it never applies; **Check** tells which rule a connection would match, and where it would go now |

Changes apply to new connections as soon as you save them; connections already open keep their route.

### Scenes

One set of rules for home, another for the office. Switch between them from the menu bar; the running scene shows a green dot.

### Activity

<table>
<tr>
<td width="68%"><img src="docs/images/en/activity.png" alt="Activity: live connections, each with its app, destination and the interface it used"></td>
<td width="32%"><img src="docs/images/en/panel.png" alt="The menu bar panel: the routing switch, the scenes, and each interface's speed"></td>
</tr>
</table>

| | |
| :-- | :-- |
| **Connections** | Live, newest first: app, destination, protocol, the rule's interface in its color, or a quiet chip where macOS routed it. Right-click to make a rule from a connection |
| **Apps** | Traffic by app, with the interfaces each used |
| **Traffic** | Bytes through each interface over the last hour, day or week |

### Network

Every interface with its addresses, router and DNS; a test of its latency and public address; and one server reached through each interface, to compare them. Interfaces can be renamed.

A mobile hotspot from Inseego, such as the MiFi M2000, shows its signal too: the bars, 5G or LTE, the band in use (n41, n71…) and the carrier. **Change hotspot IP** has it reconnect to its carrier for a new public IP. This takes the hotspot's admin password, which Pith keeps in your keychain.

### Also

- **Safe start**: if the Mac loses the internet right after routing starts, Pith undoes it.
- **Resume on launch**, **keep routing after quitting**, **open at login**, and the download speed in the menu bar.
- **Import and export** a scene's rules, or back up everything.
- **English and Simplified Chinese**, light and dark.

## Privacy

Pith reads where connections go — addresses, ports, and host names from TLS SNI, the HTTP Host or DNS — and never what they carry: no decryption, no certificates installed. Rules, the connection log and traffic totals stay on your Mac, and the log keeps as many days as Settings says. Pith sends nothing anywhere by itself; only a test you start on the Network page contacts [ipify](https://www.ipify.org) to learn an interface's public address. With a hotspot's admin password, Pith reads the hotspot's own admin page, on your local network, while the Network page is open.

## Documentation

| | |
| :-- | :-- |
| **[Architecture](docs/ARCHITECTURE.md)** | The app, the helper, the engine, and how they talk |
| **[Design](docs/DESIGN.md)** | The interface: its principles, colors and pages |
| **[Helper](internal/helper/README.md)** | What installing the helper changes, and its socket |
| **[Client](internal/client/README.md)** | The backend the interface talks to |

## License

[MIT](LICENSE)
