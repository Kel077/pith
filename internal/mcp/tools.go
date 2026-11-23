package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/Kel077/pith/internal/api"
	"github.com/Kel077/pith/internal/hotspot"
)

// tool is one tool: what an agent reads of it, and what it runs.
type tool struct {
	name  string
	title string
	about string
	input string // the JSON Schema of its arguments
	kind  kind
	does  runner
}

// runner is what a tool runs, with the type its arguments decode into.
type runner struct {
	run       func(ctx context.Context, h Host, arguments json.RawMessage) (any, error)
	arguments reflect.Type
}

// kind is what a tool does to Pith, for a client that asks its user before
// some tools and not others.
type kind int

const (
	reads    kind = iota // changes nothing
	changes              // adds to the configuration or changes it; nothing is lost
	disrupts             // removes something, or interrupts connections
)

// Tools runs the tools on a Host: the app's Caller.
type Tools struct{ Host Host }

func (t Tools) Call(ctx context.Context, name string, arguments json.RawMessage) Result {
	tl, ok := byName[name]
	if !ok {
		return Result{Text: fmt.Sprintf("this Pith has no tool %q: it may be older or newer than the program that asked", name), Failed: true}
	}
	v, err := tl.does.run(ctx, t.Host, arguments)
	if err != nil {
		return Result{Text: explain(err), Failed: true}
	}
	data, err := json.Marshal(v)
	if err != nil {
		return Result{Text: err.Error(), Failed: true}
	}
	return Result{Text: string(data)}
}

// with makes a tool's run of a function of its arguments, decoded strictly:
// a misspelled argument is an error, not one silently left out.
func with[A any](fn func(ctx context.Context, h Host, a A) (any, error)) runner {
	return runner{arguments: reflect.TypeFor[A](), run: func(ctx context.Context, h Host, raw json.RawMessage) (any, error) {
		var a A
		if raw = bytes.TrimSpace(raw); len(raw) > 0 && string(raw) != "null" {
			d := json.NewDecoder(bytes.NewReader(raw))
			d.DisallowUnknownFields()
			if err := d.Decode(&a); err != nil {
				return nil, fmt.Errorf("arguments: %s", strings.TrimPrefix(err.Error(), "json: "))
			}
		}
		return fn(ctx, h, a)
	}}
}

type none struct{}

const noArguments = `{"type": "object", "additionalProperties": false}`

// ruleProperties are the arguments add_rule and update_rule share.
const ruleProperties = `
	"action": {"type": "string", "enum": ["route", "block"], "description": "route sends matching connections out through via; block refuses them. A new rule routes."},
	"apps": {"type": "array", "items": {"type": "string"}, "description": "The apps the rule applies to, each by path (/Applications/Safari.app, /usr/bin/curl), bundle ID or name as list_apps gives them. Empty: every app."},
	"targets": {"type": "array", "items": {"type": "string"}, "description": "Destinations, any of which matches: example.com (that host alone), *.example.com (it and every subdomain), an IPv4 or IPv6 address, or a CIDR range. Empty: every destination."},
	"via": {"type": "array", "items": {"type": "string"}, "description": "The interfaces a route rule goes out through, tried in order, each by BSD name (en0), by the user's name for it or by its label, as list_interfaces gives them. A route rule needs one at least."},
	"protocol": {"type": "string", "enum": ["any", "tcp", "udp"], "description": "The transport it matches. A new rule matches any."},
	"fallback": {"type": "boolean", "description": "When none of via connects: true lets macOS route the connection, false fails it. A new rule fails it."},
	"note": {"type": "string", "description": "A note for the user, shown with the rule."},
	"enabled": {"type": "boolean", "description": "A disabled rule matches nothing. A new rule is enabled."},
	"position": {"type": "integer", "minimum": 1, "description": "Where it goes in its scene's list, 1 being first. Among route rules the first that matches wins. A new rule goes first; an updated one stays where it is."}`

const sceneProperty = `"scene": {"type": "string", "description": "A scene, by ID or name. Left out: the selected scene."}`

const interfaceProperty = `"interface": {"type": "string", "description": "An interface, by BSD name (en0), by the user's name for it or by its label."}`

var tools = []tool{
	{
		name: "status", title: "Status", kind: reads, input: noArguments,
		about: "Tells whether routing is on and by which scene, the helper's state, warnings, traffic rates, and each local proxy: whether it listens, its open connections and what goes wrong with it.",
		does:  with(status),
	},
	{
		name: "get_config", title: "Configuration", kind: reads, input: noArguments,
		about: "Returns Pith's configuration as JSON: the scenes with their rules in order (and the IDs the other tools take), the selected scene, the local proxies, the devices that may use shared proxies, the user's names for interfaces, and the settings.",
		does:  with(func(ctx context.Context, h Host, _ none) (any, error) { return h.LoadConfig(ctx) }),
	},
	{
		name: "list_interfaces", title: "Interfaces", kind: reads, input: noArguments,
		about: "Lists the Mac's network interfaces, the connected ones and the others macOS names: BSD name, label, the user's name for it (alias), kind, whether it is up, its addresses, router and DNS servers, which one is macOS's primary, and whether a local proxy can go out through it.",
		does:  with(listInterfaces),
	},
	{
		name: "list_apps", title: "Installed apps", kind: reads,
		input: `{"type": "object", "properties": {
	"search": {"type": "string", "description": "Only apps whose name, bundle ID or path contains this, ignoring case."}
}, "additionalProperties": false}`,
		about: "Lists the apps installed on the Mac with their paths and bundle IDs, to name in rules. Rules take command-line tools too, by the path of the executable.",
		does:  with(listApps),
	},
	{
		name: "check", title: "Check a connection", kind: reads,
		input: `{"type": "object", "properties": {
	"target": {"type": "string", "description": "Where the connection goes: a domain, an IP address, or a URL whose host counts."},
	"app": {"type": "string", "description": "The app that opens it, by path, bundle ID or name. Left out: an app no rule names."},
	"protocol": {"type": "string", "enum": ["tcp", "udp"], "description": "Left out: tcp."},
	` + sceneProperty + `,
	"address": {"type": "string", "description": "The IP address the connection goes to, to check a rule on addresses without a DNS lookup. Left out: what target resolves to now."}
}, "required": ["target"], "additionalProperties": false}`,
		about: "Tells what a scene's saved rules would do with a connection now: the rule that decides, the outcome (routed, fallback, system, blocked or unreachable) and the interface it would go out through. Use it after changing rules. It doesn't open the connection.",
		does:  with(check),
	},
	{
		name: "list_connections", title: "Connections", kind: reads,
		input: `{"type": "object", "properties": {
	"limit": {"type": "integer", "minimum": 1, "maximum": 500, "description": "How many, the newest first. Left out: 50."},
	"open": {"type": "boolean", "description": "Only the ones open now."},
	"app": {"type": "string", "description": "Only this app's, by path, bundle ID or name."},
	"search": {"type": "string", "description": "Only the ones whose app name, domain or address contains this, ignoring case."},
	"minutes": {"type": "number", "exclusiveMinimum": 0, "description": "Only the ones that started in the last so many minutes."}
}, "additionalProperties": false}`,
		about: "Lists connections Pith handled while routing, the newest first: the app, the destination, the rule that decided, the outcome, the interface and the bytes. Connections through local proxies aren't logged here: status counts those.",
		does:  with(listConnections),
	},
	{
		name: "traffic", title: "Traffic", kind: reads,
		input: `{"type": "object", "properties": {
	"hours": {"type": "number", "exclusiveMinimum": 0, "description": "The period, counted back from now. Left out: 24."}
}, "additionalProperties": false}`,
		about: "Sums the bytes of the connections Pith routed over a period, by app and by interface, the most first.",
		does:  with(traffic),
	},
	{
		name: "test_interface", title: "Test an interface", kind: reads,
		input: `{"type": "object", "properties": {
	` + interfaceProperty + `,
	"target": {"type": "string", "description": "A host:port to open over TCP. Left out: Pith's HTTPS test, which also finds the interface's public IP addresses."}
}, "required": ["interface"], "additionalProperties": false}`,
		about: "Connects through one interface, whatever the rules say: tells whether it reaches the internet, how long it took, and the public IPv4 and IPv6 addresses the internet sees it by.",
		does:  with(testInterface),
	},
	{
		name: "speed_test", title: "Speed test", kind: reads,
		input: `{"type": "object", "properties": {
	` + interfaceProperty + `
}, "required": ["interface"], "additionalProperties": false}`,
		about: "Measures the latency, download and upload speeds through one interface against a nearby speed test server. It takes some ten seconds and uses about 33 MB, which counts on a metered hotspot: run it when the user asks for speeds.",
		does:  with(speedTest),
	},
	{
		name: "gateway_status", title: "Hotspot or router", kind: reads,
		input: `{"type": "object", "properties": {
	` + interfaceProperty + `
}, "required": ["interface"], "additionalProperties": false}`,
		about: "Reads the mobile hotspot or the router behind an interface, when it is a model Pith talks to: its model, whether it is connected, and a hotspot's signal and cell or a router's public IPv4 address. Locked means Pith needs the device's admin password, which the user enters on Pith's Network page. Whatever this can read, change_ip can change the address of.",
		does:  with(gatewayStatus),
	},
	{
		name: "start_routing", title: "Start routing", kind: changes,
		input: `{"type": "object", "properties": {
	` + sceneProperty + `
}, "additionalProperties": false}`,
		about: "Turns routing on by a scene's rules, or switches the scene that routes, and selects that scene. It needs Pith's helper, which the user installs in Pith. With safe start on, a start that cuts the internet is undone.",
		does:  with(startRouting),
	},
	{
		name: "stop_routing", title: "Stop routing", kind: disrupts, input: noArguments,
		about: "Turns routing off: macOS routes every connection again, and connections Pith routed end. Local proxies keep working.",
		does:  with(stopRouting),
	},
	{
		name: "add_rule", title: "Add a rule", kind: changes,
		input: `{"type": "object", "properties": {
	` + sceneProperty + `,` + ruleProperties + `
}, "additionalProperties": false}`,
		about: "Adds a rule to a scene and returns it with its ID. When that scene routes, it applies at once: open connections it decides differently are closed to reconnect. Example: {\"apps\": [\"Safari\"], \"targets\": [\"*.example.com\"], \"via\": [\"en0\"]}.",
		does:  with(addRule),
	},
	{
		name: "update_rule", title: "Change a rule", kind: changes,
		input: `{"type": "object", "properties": {
	"id": {"type": "string", "description": "The rule's ID, from get_config."},` + ruleProperties + `
}, "required": ["id"], "additionalProperties": false}`,
		about: "Changes a rule: only the arguments given change, and a list given replaces the rule's whole list. To turn a rule off without losing it, give enabled false. It applies at once when its scene routes.",
		does:  with(updateRule),
	},
	{
		name: "remove_rule", title: "Remove a rule", kind: disrupts,
		input: `{"type": "object", "properties": {
	"id": {"type": "string", "description": "The rule's ID, from get_config."}
}, "required": ["id"], "additionalProperties": false}`,
		about: "Removes a rule for good, and returns what it was. update_rule with enabled false keeps it instead.",
		does:  with(removeRule),
	},
	{
		name: "add_scene", title: "Add a scene", kind: changes,
		input: `{"type": "object", "properties": {
	"name": {"type": "string", "description": "Its name."},
	"copy_of": {"type": "string", "description": "A scene, by ID or name, whose rules it starts with. Left out: no rules."}
}, "required": ["name"], "additionalProperties": false}`,
		about: "Adds a scene, empty or a copy of another's rules, and returns it with its ID. It routes nothing until start_routing names it.",
		does:  with(addScene),
	},
	{
		name: "rename_scene", title: "Rename a scene", kind: changes,
		input: `{"type": "object", "properties": {
	"scene": {"type": "string", "description": "The scene, by ID or name."},
	"name": {"type": "string", "description": "Its new name."}
}, "required": ["scene", "name"], "additionalProperties": false}`,
		about: "Gives a scene another name.",
		does:  with(renameScene),
	},
	{
		name: "remove_scene", title: "Remove a scene", kind: disrupts,
		input: `{"type": "object", "properties": {
	"scene": {"type": "string", "description": "The scene, by ID or name."}
}, "required": ["scene"], "additionalProperties": false}`,
		about: "Removes a scene and its rules for good. Pith keeps one scene at least. Routing stops when it is the scene that routes, and a local proxy that went out by its rules goes out by the selected scene's.",
		does:  with(removeScene),
	},
	{
		name: "set_proxy", title: "Set a local proxy", kind: changes,
		input: `{"type": "object", "properties": {
	"port": {"type": "integer", "minimum": 1024, "maximum": 65535, "description": "Its port on 127.0.0.1, which identifies it."},
	"via": {"type": "string", "description": "It goes out through this one interface, by BSD name, the user's name for it or its label: when the interface is down, connections fail rather than use another."},
	"upstream": {"type": "string", "description": "It goes out through this other proxy, the host:port of a SOCKS5 proxy without a password, as a port another Mac's Pith shares."},
	"scene": {"type": "string", "description": "It goes out by this scene's rules, by ID or name, each connection where they send it, whether or not that scene routes. Rules that name apps are left out."},
	"enabled": {"type": "boolean", "description": "Off, the port is closed and the proxy kept. A new proxy is enabled."},
	"shared": {"type": "boolean", "description": "On, the devices allow_device lists can use it too, at the Mac's own addresses, without a password."},
	"change_ip": {"type": "boolean", "description": "On, every app on the Mac and every device the proxy is shared with may change the public IP of what it goes out through, with a POST of /change-ip to the port."}
}, "required": ["port"], "additionalProperties": false}`,
		about: "Adds the local proxy on a port, or changes the one there: a SOCKS5 and HTTP proxy for apps and browsers that take one, which works whether routing is on or off. Give one of via, upstream and scene for a new proxy, or to change what an existing one goes out through; other arguments left out stay as they are. Changing what it goes out through, or whether it is shared, closes its connections.",
		does:  with(setProxy),
	},
	{
		name: "remove_proxy", title: "Remove a local proxy", kind: disrupts,
		input: `{"type": "object", "properties": {
	"port": {"type": "integer", "description": "The proxy's port."}
}, "required": ["port"], "additionalProperties": false}`,
		about: "Removes the local proxy on a port: the port closes, and its connections end. set_proxy with enabled false keeps it instead.",
		does:  with(removeProxy),
	},
	{
		name: "allow_device", title: "Allow a device", kind: changes,
		input: `{"type": "object", "properties": {
	"address": {"type": "string", "description": "Its IPv4 address as the Mac sees it connect, or a CIDR range of them of at least 8 bits: 192.0.2.10, 192.0.2.0/24."},
	"name": {"type": "string", "description": "The user's name for it."}
}, "required": ["address"], "additionalProperties": false}`,
		about: "Lets another device, or a range of them, use the local proxies that are shared, or renames one already listed. A device reaches through a shared proxy what the Mac reaches, its local network included, without a password: list only devices the user named. status lists the devices that connected, refused ones included.",
		does:  with(allowDevice),
	},
	{
		name: "remove_device", title: "Remove a device", kind: disrupts,
		input: `{"type": "object", "properties": {
	"address": {"type": "string", "description": "The address or range it is listed by."}
}, "required": ["address"], "additionalProperties": false}`,
		about: "Stops letting a device use the shared local proxies: its connections end.",
		does:  with(removeDevice),
	},
	{
		name: "change_ip", title: "Change a public IP", kind: disrupts,
		input: `{"type": "object", "properties": {
	"interface": {"type": "string", "description": "The interface whose hotspot or router gets a new address, by BSD name, the user's name for it or its label."},
	"port": {"type": "integer", "description": "Instead of an interface: the port of a local proxy, to change the address of whatever it goes out through, another Mac's Pith included."}
}, "additionalProperties": false}`,
		about: "Reconnects a mobile hotspot or renews a router's IPv4 lease. Give an interface or a local proxy's port. Returns the public addresses before and after and the elapsed seconds. ipv4Changed and ipv6Changed compare each family when both addresses were observed. changed is true when either family changed. Connections through that network end. A router also takes other devices behind it offline and needs another connected network to carry the Mac. The provider may return the same address. Use only when the user asks for a new IP and allow time for reconnection.",
		does:  with(changeIP),
	},
}

// byName finds a tool; listed is what tools/list answers.
var (
	byName = map[string]*tool{}
	listed []map[string]any
)

func init() {
	for i := range tools {
		t := &tools[i]
		byName[t.name] = t
		listed = append(listed, map[string]any{
			"name":        t.name,
			"title":       t.title,
			"description": t.about,
			"inputSchema": json.RawMessage(t.input),
			"annotations": map[string]any{
				"title":           t.title,
				"readOnlyHint":    t.kind == reads,
				"destructiveHint": t.kind == disrupts,
				"idempotentHint":  t.kind == reads,
				"openWorldHint":   false,
			},
		})
	}
}

// ---- errors -----------------------------------------------------------------

// hints say what to do about an error of the backend's, where its code and
// detail don't.
var hints = map[api.ErrorCode]string{
	api.ErrHelperMissing:       "Pith's helper isn't installed. The user installs it in Pith, which asks for an administrator's password: turning routing on there offers it.",
	api.ErrHelperOutdated:      "Pith's helper needs an update, which the user approves in Pith with an administrator's password.",
	api.ErrHelperUntrusted:     "Pith can't verify its helper. The user repairs it in Pith's settings.",
	api.ErrHelperNotResponding: "The helper is installed and doesn't answer. The user repairs it in Pith's settings.",
	api.ErrNoScene:             "No scene has that ID: get_config lists them.",
	api.ErrRouteConflict:       "Another VPN or network tool holds routes Pith needs.",
	api.ErrNetworkLost:         "Routing cut the internet, so Pith undid the start.",
	api.ErrUnsupported:         "Pith can't do that with what is behind that interface or port.",
	api.ErrWrongPassword:       "Pith has no admin password that the device behind that interface accepts. The user enters it on Pith's Network page.",
	api.ErrNoStandby:           "Changing a router's address needs another connected interface with internet, to carry the Mac meanwhile.",
	api.ErrBusy:                "Another interface's public address is being changed. Try again once that is done.",
	api.ErrNotAllowed:          "The other Pith's port doesn't let what connects to it change its address: its user turns that on there.",
}

// explain puts an error in words for an agent.
func explain(err error) string {
	var e *api.Error
	if !errors.As(err, &e) {
		return err.Error()
	}
	text := string(e.Code)
	if e.Detail != "" {
		text += ": " + e.Detail
	}
	if hint := hints[e.Code]; hint != "" {
		text += ". " + hint
	}
	return text
}

// ---- names ------------------------------------------------------------------

// findScene finds a scene by its ID or, ignoring case, by its name; the
// selected one for "".
func findScene(cfg *api.Config, ref string) (*api.Scene, error) {
	if ref == "" {
		ref = cfg.Selected
	}
	for i := range cfg.Scenes {
		if cfg.Scenes[i].ID == ref {
			return &cfg.Scenes[i], nil
		}
	}
	var found *api.Scene
	var all []string
	for i := range cfg.Scenes {
		s := &cfg.Scenes[i]
		all = append(all, fmt.Sprintf("%s (%q)", s.ID, s.Name))
		if s.Name != "" && strings.EqualFold(s.Name, ref) {
			if found != nil {
				return nil, fmt.Errorf("several scenes are named %q: give the ID, one of %s", ref, strings.Join(all, ", "))
			}
			found = s
		}
	}
	if found == nil {
		return nil, fmt.Errorf("no scene %q: the scenes are %s", ref, strings.Join(all, ", "))
	}
	return found, nil
}

// findRule finds a rule by its ID in any scene.
func findRule(cfg *api.Config, id string) (*api.Scene, int, error) {
	for i := range cfg.Scenes {
		for k := range cfg.Scenes[i].Rules {
			if cfg.Scenes[i].Rules[k].ID == id {
				return &cfg.Scenes[i], k, nil
			}
		}
	}
	return nil, 0, fmt.Errorf("no rule has the ID %q: get_config lists the rules", id)
}

// network is the interfaces with the user's names for them.
type network struct {
	ifaces  []api.Interface
	aliases map[string]string
}

func readNetwork(ctx context.Context, h Host) (network, error) {
	ifaces, err := h.Interfaces(ctx)
	if err != nil {
		return network{}, err
	}
	cfg, err := h.LoadConfig(ctx)
	if err != nil {
		return network{}, err
	}
	return network{ifaces: ifaces, aliases: cfg.Aliases}, nil
}

// shown tells the interfaces Pith shows from the dozens a Mac has: the
// connected ones, and those macOS gives a name of their own, but for a
// VPN's tunnel that isn't up. The others still go by their BSD names.
func shown(i *api.Interface) bool {
	return i.Up || i.Label != "" && i.Label != i.Name && i.Kind != api.KindVPN
}

func refOf(i *api.Interface) api.InterfaceRef {
	return api.InterfaceRef{Name: i.Name, Label: i.Label, HardwareAddr: i.HardwareAddr}
}

// find finds an interface by its BSD name, else by the user's name for it,
// else by its label, ignoring case.
func (n network) find(ref string) (*api.Interface, error) {
	ref = strings.TrimSpace(ref)
	for i := range n.ifaces {
		if n.ifaces[i].Name == ref {
			return &n.ifaces[i], nil
		}
	}
	var found []*api.Interface
	for _, by := range []func(*api.Interface) string{
		func(i *api.Interface) string { return n.aliases[refOf(i).Key()] },
		func(i *api.Interface) string { return i.Label },
		func(i *api.Interface) string { return i.HardwarePort },
	} {
		for i := range n.ifaces {
			if name := by(&n.ifaces[i]); name != "" && strings.EqualFold(name, ref) {
				found = append(found, &n.ifaces[i])
			}
		}
		if len(found) > 0 {
			break
		}
	}
	if len(found) == 1 {
		return found[0], nil
	}
	var names []string
	list := n.ifaces
	if len(found) > 1 {
		list = nil
		for _, i := range found {
			list = append(list, *i)
		}
	}
	for i := range list {
		if len(found) > 1 || shown(&list[i]) {
			names = append(names, n.describe(&list[i]))
		}
	}
	if len(found) > 1 {
		return nil, fmt.Errorf("several interfaces are named %q: give the BSD name, one of %s", ref, strings.Join(names, ", "))
	}
	return nil, fmt.Errorf("no interface %q: the interfaces are %s", ref, strings.Join(names, ", "))
}

func (n network) describe(i *api.Interface) string {
	name := i.Label
	if alias := n.aliases[refOf(i).Key()]; alias != "" {
		name = alias
	}
	if name == "" {
		return i.Name
	}
	return fmt.Sprintf("%s (%s)", i.Name, name)
}

// findApp finds an app by its path, its bundle ID or its name, ignoring
// case but for paths. A path that names no installed app is taken as it is:
// a command-line tool, or an app elsewhere than where apps are installed.
func findApp(apps []api.AppRef, ref string) (api.AppRef, error) {
	ref = strings.TrimSpace(ref)
	if strings.HasPrefix(ref, "/") {
		path := strings.TrimRight(ref, "/")
		for _, a := range apps {
			if a.Path == path {
				return a, nil
			}
		}
		name := path[strings.LastIndex(path, "/")+1:]
		return api.AppRef{Path: path, Name: strings.TrimSuffix(name, ".app")}, nil
	}
	var found []api.AppRef
	for _, a := range apps {
		if strings.EqualFold(a.BundleID, ref) || strings.EqualFold(a.Name, ref) || strings.EqualFold(a.Name+".app", ref) {
			found = append(found, a)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return api.AppRef{}, fmt.Errorf("no installed app is named %q: list_apps finds apps, and a command-line tool goes by the path of its executable", ref)
	}
	var paths []string
	for _, a := range found {
		paths = append(paths, a.Path)
	}
	return api.AppRef{}, fmt.Errorf("several apps are named %q: give the path, one of %s", ref, strings.Join(paths, ", "))
}

// ---- reading ----------------------------------------------------------------

func status(ctx context.Context, h Host, _ none) (any, error) {
	st, err := h.Status(ctx)
	if err != nil {
		return nil, err
	}
	// What a capture left behind is for diagnosing Pith, not for working it.
	st.LastCapture, st.RouteDiagnostics = nil, nil
	return st, nil
}

type interfaceView struct {
	api.Interface
	Alias    string `json:"alias,omitempty"`
	CanProxy bool   `json:"canProxy"`
}

func listInterfaces(ctx context.Context, h Host, _ none) (any, error) {
	n, err := readNetwork(ctx, h)
	if err != nil {
		return nil, err
	}
	out := []interfaceView{}
	for i := range n.ifaces {
		if iface := &n.ifaces[i]; shown(iface) {
			out = append(out, interfaceView{Interface: *iface, Alias: n.aliases[refOf(iface).Key()], CanProxy: iface.CanProxy()})
		}
	}
	return out, nil
}

// maxApps bounds what list_apps answers: a Mac has hundreds of apps.
const maxApps = 100

func listApps(ctx context.Context, h Host, a struct {
	Search string `json:"search"`
}) (any, error) {
	apps, err := h.Apps(ctx)
	if err != nil {
		return nil, err
	}
	out := []api.AppRef{}
	search := strings.ToLower(strings.TrimSpace(a.Search))
	more := 0
	for _, app := range apps {
		if search != "" && !strings.Contains(strings.ToLower(app.Name+"\n"+app.BundleID+"\n"+app.Path), search) {
			continue
		}
		if len(out) == maxApps {
			more++
			continue
		}
		out = append(out, app)
	}
	res := map[string]any{"apps": out}
	if more > 0 {
		res["more"] = fmt.Sprintf("%d more: narrow them with search", more)
	}
	return res, nil
}

func check(ctx context.Context, h Host, a struct {
	Target   string `json:"target"`
	App      string `json:"app"`
	Protocol string `json:"protocol"`
	Scene    string `json:"scene"`
	Address  string `json:"address"`
}) (any, error) {
	if strings.TrimSpace(a.Target) == "" {
		return nil, errors.New("target: a domain, an IP address or a URL")
	}
	cfg, err := h.LoadConfig(ctx)
	if err != nil {
		return nil, err
	}
	scene, err := findScene(&cfg, a.Scene)
	if err != nil {
		return nil, err
	}
	in := api.CheckInput{SceneID: scene.ID, Target: a.Target, Address: a.Address, Protocol: api.TCP}
	switch a.Protocol {
	case "", "tcp":
	case "udp":
		in.Protocol = api.UDP
	default:
		return nil, errors.New("protocol: tcp or udp")
	}
	if a.App != "" {
		apps, err := h.Apps(ctx)
		if err != nil {
			return nil, err
		}
		if in.App, err = findApp(apps, a.App); err != nil {
			return nil, err
		}
	}
	res, err := h.Check(ctx, in)
	if err != nil {
		return nil, err
	}
	// Of the scene's rules, the ones that match tell the story: the one that
	// decides, and those it shadows.
	res.Rules = slices.DeleteFunc(res.Rules, func(r api.RuleCheck) bool {
		return r.Result != api.CheckMatched && r.Result != api.CheckShadowed
	})
	out := map[string]any{"scene": scene.ID, "outcome": res.Outcome, "matching": res.Rules}
	if res.RuleID != "" {
		if of, k, err := findRule(&cfg, res.RuleID); err == nil {
			out["rule"] = of.Rules[k]
		}
	}
	if res.Interface != "" {
		out["interface"] = res.Interface
	}
	if len(res.Addresses) > 0 {
		out["addresses"] = res.Addresses
	}
	return out, nil
}

type connectionView struct {
	ID        uint64        `json:"id"`
	Started   time.Time     `json:"started"`
	Ended     time.Time     `json:"ended,omitzero"`
	EndReason api.EndReason `json:"endReason,omitempty"`
	App       string        `json:"app"`
	Process   string        `json:"process,omitempty"` // when it isn't the app's own
	Protocol  api.Protocol  `json:"protocol"`
	Remote    string        `json:"remote"`
	Domain    string        `json:"domain,omitempty"`
	RuleID    string        `json:"ruleId,omitempty"`
	Outcome   api.Outcome   `json:"outcome"`
	Interface string        `json:"interface,omitempty"`
	Problem   string        `json:"problem,omitempty"`
	Sent      uint64        `json:"sent"`
	Received  uint64        `json:"received"`
}

func listConnections(ctx context.Context, h Host, a struct {
	Limit   int     `json:"limit"`
	Open    bool    `json:"open"`
	App     string  `json:"app"`
	Search  string  `json:"search"`
	Minutes float64 `json:"minutes"`
}) (any, error) {
	q := api.ConnectionQuery{Limit: a.Limit, Open: a.Open, Search: a.Search}
	if q.Limit <= 0 {
		q.Limit = 50
	}
	q.Limit = min(q.Limit, 500)
	if a.Minutes > 0 {
		q.Since = time.Now().Add(-time.Duration(a.Minutes * float64(time.Minute)))
	}
	if a.App != "" {
		apps, err := h.Apps(ctx)
		if err != nil {
			return nil, err
		}
		app, err := findApp(apps, a.App)
		if err != nil {
			return nil, err
		}
		q.App = app.Path
	}
	conns, err := h.Connections(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make([]connectionView, len(conns))
	for i, c := range conns {
		v := connectionView{ID: c.ID, Started: c.Started, Ended: c.Ended, EndReason: c.EndReason, App: c.App.Name,
			Protocol: c.Protocol, Remote: c.Remote, Domain: c.Domain, RuleID: c.RuleID, Outcome: c.Outcome,
			Interface: c.Interface, Problem: c.Problem, Sent: c.Sent, Received: c.Received}
		if v.App == "" {
			v.App = c.App.Path
		}
		if c.Process.Path != c.App.Path && c.Process.Name != v.App {
			v.Process = c.Process.Name
		}
		out[i] = v
	}
	return out, nil
}

// maxTrafficApps bounds the apps traffic answers with.
const maxTrafficApps = 50

func traffic(ctx context.Context, h Host, a struct {
	Hours float64 `json:"hours"`
}) (any, error) {
	if a.Hours <= 0 {
		a.Hours = 24
	}
	t, err := h.Traffic(ctx, api.TrafficQuery{Since: time.Now().Add(-time.Duration(a.Hours * float64(time.Hour)))})
	if err != nil {
		return nil, err
	}
	t.Series = nil
	if len(t.Apps) > maxTrafficApps {
		t.Apps = t.Apps[:maxTrafficApps]
	}
	return t, nil
}

// milliseconds is a duration as an agent reads it.
func milliseconds(d time.Duration) float64 {
	return float64(d.Round(100*time.Microsecond)) / float64(time.Millisecond)
}

type probeView struct {
	Interface  string  `json:"interface"`
	Target     string  `json:"target,omitempty"`
	Address    string  `json:"address,omitempty"`
	OK         bool    `json:"ok"`
	LatencyMS  float64 `json:"latencyMs"`
	PublicIPv4 string  `json:"publicIPv4,omitempty"`
	PublicIPv6 string  `json:"publicIPv6,omitempty"`
	Problem    string  `json:"problem,omitempty"`
}

func viewProbe(p api.Probe) probeView {
	return probeView{Interface: p.Interface, Target: p.Target, Address: p.Address, OK: p.OK, LatencyMS: milliseconds(p.Latency),
		PublicIPv4: p.PublicIPv4, PublicIPv6: p.PublicIPv6, Problem: p.Problem}
}

// interfaceNamed finds the interface a tool's argument names.
func interfaceNamed(ctx context.Context, h Host, ref string) (*api.Interface, error) {
	if strings.TrimSpace(ref) == "" {
		return nil, errors.New("interface: its BSD name, the user's name for it or its label")
	}
	n, err := readNetwork(ctx, h)
	if err != nil {
		return nil, err
	}
	return n.find(ref)
}

func testInterface(ctx context.Context, h Host, a struct {
	Interface string `json:"interface"`
	Target    string `json:"target"`
}) (any, error) {
	iface, err := interfaceNamed(ctx, h, a.Interface)
	if err != nil {
		return nil, err
	}
	p, err := h.Probe(ctx, iface.Name, a.Target)
	if err != nil {
		return nil, err
	}
	return viewProbe(p), nil
}

func speedTest(ctx context.Context, h Host, a struct {
	Interface string `json:"interface"`
}) (any, error) {
	iface, err := interfaceNamed(ctx, h, a.Interface)
	if err != nil {
		return nil, err
	}
	t, err := h.SpeedTest(ctx, iface.Name)
	if err != nil {
		return nil, err
	}
	mbps := func(bits int64) float64 { return float64(bits/10_000) / 100 }
	return struct {
		Interface    string  `json:"interface"`
		OK           bool    `json:"ok"`
		Server       string  `json:"server,omitempty"`
		LatencyMS    float64 `json:"latencyMs"`
		DownloadMbps float64 `json:"downloadMbps"`
		UploadMbps   float64 `json:"uploadMbps"`
		PublicIP     string  `json:"publicIP,omitempty"`
		Problem      string  `json:"problem,omitempty"`
	}{t.Interface, t.OK, t.Server, milliseconds(t.Latency), mbps(t.Download), mbps(t.Upload), t.PublicIP, t.Problem}, nil
}

// gateway reads what is behind an interface: its hotspot, else its router.
// Both nil with ErrUnsupported when Pith talks to neither.
func gateway(ctx context.Context, h Host, name string) (*api.Hotspot, *api.Router, error) {
	hs, err := h.Hotspot(ctx, name)
	if err == nil {
		return &hs, nil, nil
	}
	if api.Code(err) != api.ErrUnsupported {
		return nil, nil, err
	}
	r, err := h.Router(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	return nil, &r, nil
}

func gatewayStatus(ctx context.Context, h Host, a struct {
	Interface string `json:"interface"`
}) (any, error) {
	iface, err := interfaceNamed(ctx, h, a.Interface)
	if err != nil {
		return nil, err
	}
	hs, r, err := gateway(ctx, h, iface.Name)
	switch {
	case err != nil:
		return nil, err
	case hs != nil:
		return map[string]any{"hotspot": hs}, nil
	}
	return map[string]any{"router": r}, nil
}

// ---- routing ----------------------------------------------------------------

type routingView struct {
	State   api.State  `json:"state"`
	SceneID string     `json:"sceneId,omitempty"`
	Scene   string     `json:"scene,omitempty"` // its name
	Error   *api.Error `json:"error,omitempty"`
}

func routing(ctx context.Context, h Host) (any, error) {
	st, err := h.Status(ctx)
	if err != nil {
		return nil, err
	}
	v := routingView{State: st.State, SceneID: st.SceneID, Error: st.Error}
	if cfg, err := h.LoadConfig(ctx); err == nil && st.SceneID != "" {
		if s, err := findScene(&cfg, st.SceneID); err == nil {
			v.Scene = s.Name
		}
	}
	return v, nil
}

func startRouting(ctx context.Context, h Host, a struct {
	Scene string `json:"scene"`
}) (any, error) {
	cfg, err := h.LoadConfig(ctx)
	if err != nil {
		return nil, err
	}
	scene, err := findScene(&cfg, a.Scene)
	if err != nil {
		return nil, err
	}
	if err := h.Start(ctx, scene.ID); err != nil {
		return nil, err
	}
	return routing(ctx, h)
}

func stopRouting(ctx context.Context, h Host, _ none) (any, error) {
	if err := h.Stop(ctx); err != nil {
		return nil, err
	}
	return routing(ctx, h)
}

// ---- rules ------------------------------------------------------------------

// ruleFields are what add_rule and update_rule set of a rule: nil leaves a
// field as it is.
type ruleFields struct {
	Action   *string   `json:"action"`
	Apps     *[]string `json:"apps"`
	Targets  *[]string `json:"targets"`
	Via      *[]string `json:"via"`
	Protocol *string   `json:"protocol"`
	Fallback *bool     `json:"fallback"`
	Note     *string   `json:"note"`
	Enabled  *bool     `json:"enabled"`
	Position *int      `json:"position"`
}

// ruleChange is ruleFields with its names found: what apply needs, read
// before the configuration is edited.
type ruleChange struct {
	ruleFields
	apps    []api.AppRef
	targets []string
	via     []api.InterfaceRef
}

func (f ruleFields) find(ctx context.Context, h Host) (ruleChange, error) {
	c := ruleChange{ruleFields: f}
	if f.Apps != nil && len(*f.Apps) > 0 {
		apps, err := h.Apps(ctx)
		if err != nil {
			return c, err
		}
		for _, ref := range *f.Apps {
			app, err := findApp(apps, ref)
			if err != nil {
				return c, err
			}
			c.apps = append(c.apps, app)
		}
	}
	if f.Targets != nil {
		for _, s := range *f.Targets {
			t, err := api.ParseTarget(s)
			if err != nil {
				return c, fmt.Errorf("target %q is %v: write example.com, *.example.com, an IP address or a CIDR range", s, err)
			}
			c.targets = append(c.targets, t.String())
		}
	}
	if f.Via != nil && len(*f.Via) > 0 {
		n, err := readNetwork(ctx, h)
		if err != nil {
			return c, err
		}
		for _, ref := range *f.Via {
			iface, err := n.find(ref)
			if err != nil {
				return c, err
			}
			c.via = append(c.via, refOf(iface))
		}
	}
	return c, nil
}

// apply sets the fields given on r, and tells what is wrong with the rule
// they leave.
func (c ruleChange) apply(r *api.Rule) error {
	if c.Action != nil {
		r.Action = api.Action(*c.Action)
	}
	if c.Apps != nil {
		r.Apps = c.apps
	}
	if c.Targets != nil {
		r.Targets = c.targets
	}
	if c.Via != nil {
		r.Via = c.via
	}
	if c.Protocol != nil {
		r.Protocol = api.Protocol(*c.Protocol)
	}
	if c.Fallback != nil {
		r.Fallback = *c.Fallback
	}
	if c.Note != nil {
		r.Note = *c.Note
	}
	if c.Enabled != nil {
		r.Enabled = *c.Enabled
	}
	if r.Action == api.Block {
		r.Via, r.Fallback = nil, false // a block rule uses neither
	}
	for _, p := range r.Problems() {
		switch p.Code {
		case api.ProblemNoVia:
			return errors.New("a route rule needs via: the interfaces it goes out through, as list_interfaces names them")
		case api.ProblemBadAction:
			return errors.New("action: route or block")
		case api.ProblemBadProtocol:
			return errors.New("protocol: any, tcp or udp")
		case api.ProblemDuplicate:
			return fmt.Errorf("%s: number %d is there twice", p.Field, p.Index+1)
		default:
			return fmt.Errorf("%s: number %d: %s", p.Field, p.Index+1, p.Code)
		}
	}
	return nil
}

// place puts the rule at index from of rules at a 1-based position.
func place(rules []api.Rule, from, position int) ([]api.Rule, int) {
	to := min(max(position, 1), len(rules)) - 1
	if to == from {
		return rules, from
	}
	r := rules[from]
	rules = slices.Delete(rules, from, from+1)
	return slices.Insert(rules, to, r), to
}

func ruleView(scene *api.Scene, k int) map[string]any {
	return map[string]any{"scene": scene.ID, "position": k + 1, "rule": scene.Rules[k]}
}

func addRule(ctx context.Context, h Host, a struct {
	Scene string `json:"scene"`
	ruleFields
}) (any, error) {
	change, err := a.find(ctx, h)
	if err != nil {
		return nil, err
	}
	var out any
	_, err = h.Edit(ctx, func(cfg *api.Config) error {
		scene, err := findScene(cfg, a.Scene)
		if err != nil {
			return err
		}
		r := api.Rule{ID: api.NewID(), Enabled: true, Protocol: api.AnyProtocol, Action: api.Route}
		if err := change.apply(&r); err != nil {
			return err
		}
		// First, as in Pith: a new rule is most often an exception to the
		// ones below it.
		scene.Rules = slices.Insert(scene.Rules, 0, r)
		k := 0
		if a.Position != nil {
			scene.Rules, k = place(scene.Rules, 0, *a.Position)
		}
		out = ruleView(scene, k)
		return nil
	})
	return out, err
}

func updateRule(ctx context.Context, h Host, a struct {
	ID string `json:"id"`
	ruleFields
}) (any, error) {
	change, err := a.find(ctx, h)
	if err != nil {
		return nil, err
	}
	var out any
	_, err = h.Edit(ctx, func(cfg *api.Config) error {
		scene, k, err := findRule(cfg, a.ID)
		if err != nil {
			return err
		}
		if err := change.apply(&scene.Rules[k]); err != nil {
			return err
		}
		if a.Position != nil {
			scene.Rules, k = place(scene.Rules, k, *a.Position)
		}
		out = ruleView(scene, k)
		return nil
	})
	return out, err
}

func removeRule(ctx context.Context, h Host, a struct {
	ID string `json:"id"`
}) (any, error) {
	var out any
	_, err := h.Edit(ctx, func(cfg *api.Config) error {
		scene, k, err := findRule(cfg, a.ID)
		if err != nil {
			return err
		}
		out = map[string]any{"scene": scene.ID, "removed": scene.Rules[k]}
		scene.Rules = slices.Delete(scene.Rules, k, k+1)
		return nil
	})
	return out, err
}

// ---- scenes -----------------------------------------------------------------

type sceneView struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Rules int    `json:"rules"`
}

func viewScene(s *api.Scene) sceneView { return sceneView{ID: s.ID, Name: s.Name, Rules: len(s.Rules)} }

func addScene(ctx context.Context, h Host, a struct {
	Name   string `json:"name"`
	CopyOf string `json:"copy_of"`
}) (any, error) {
	name := strings.TrimSpace(a.Name)
	if name == "" {
		return nil, errors.New("name: the scene's name")
	}
	var out sceneView
	_, err := h.Edit(ctx, func(cfg *api.Config) error {
		s := api.Scene{ID: api.NewID(), Name: name, Rules: []api.Rule{}}
		if a.CopyOf != "" {
			src, err := findScene(cfg, a.CopyOf)
			if err != nil {
				return err
			}
			for _, r := range src.Rules {
				r.Apps, r.Targets, r.Via = slices.Clone(r.Apps), slices.Clone(r.Targets), slices.Clone(r.Via)
				r.ID = api.NewID()
				s.Rules = append(s.Rules, r)
			}
		}
		cfg.Scenes = append(cfg.Scenes, s)
		out = viewScene(&s)
		return nil
	})
	return out, err
}

func renameScene(ctx context.Context, h Host, a struct {
	Scene string `json:"scene"`
	Name  string `json:"name"`
}) (any, error) {
	name := strings.TrimSpace(a.Name)
	if a.Scene == "" || name == "" {
		return nil, errors.New("scene and name: the scene, by ID or name, and its new name")
	}
	var out sceneView
	_, err := h.Edit(ctx, func(cfg *api.Config) error {
		s, err := findScene(cfg, a.Scene)
		if err != nil {
			return err
		}
		s.Name = name
		out = viewScene(s)
		return nil
	})
	return out, err
}

func removeScene(ctx context.Context, h Host, a struct {
	Scene string `json:"scene"`
}) (any, error) {
	if a.Scene == "" {
		return nil, errors.New("scene: the scene to remove, by ID or name")
	}
	cfg, err := h.LoadConfig(ctx)
	if err != nil {
		return nil, err
	}
	scene, err := findScene(&cfg, a.Scene)
	if err != nil {
		return nil, err
	}
	if len(cfg.Scenes) < 2 {
		return nil, errors.New("Pith keeps one scene at least: this is the last")
	}
	id := scene.ID
	// As in Pith: removing the scene that routes turns routing off.
	if st, err := h.Status(ctx); err == nil && st.SceneID == id && (st.State == api.Running || st.State == api.Starting) {
		if err := h.Stop(ctx); err != nil {
			return nil, err
		}
	}
	var out any
	_, err = h.Edit(ctx, func(cfg *api.Config) error {
		k := slices.IndexFunc(cfg.Scenes, func(s api.Scene) bool { return s.ID == id })
		if k < 0 {
			return fmt.Errorf("no scene %q", id)
		}
		if len(cfg.Scenes) < 2 {
			return errors.New("Pith keeps one scene at least: this is the last")
		}
		removed := viewScene(&cfg.Scenes[k])
		cfg.Scenes = slices.Delete(cfg.Scenes, k, k+1)
		if cfg.Selected == id {
			cfg.Selected = cfg.Scenes[0].ID
		}
		// A local proxy that went out by its rules goes out by those of the
		// scene now selected.
		for i := range cfg.Proxies {
			if cfg.Proxies[i].Scene == id {
				cfg.Proxies[i].Scene = cfg.Selected
			}
		}
		out = map[string]any{"removed": removed, "selected": cfg.Selected}
		return nil
	})
	return out, err
}

// ---- local proxies and devices ----------------------------------------------

func setProxy(ctx context.Context, h Host, a struct {
	Port     int     `json:"port"`
	Via      *string `json:"via"`
	Upstream *string `json:"upstream"`
	Scene    *string `json:"scene"`
	Enabled  *bool   `json:"enabled"`
	Shared   *bool   `json:"shared"`
	ChangeIP *bool   `json:"change_ip"`
}) (any, error) {
	exits := 0
	for _, s := range []*string{a.Via, a.Upstream, a.Scene} {
		if s != nil && *s != "" {
			exits++
		}
	}
	if exits > 1 {
		return nil, errors.New("give one of via, upstream and scene: a proxy goes out one way")
	}
	var via api.InterfaceRef
	if a.Via != nil && *a.Via != "" {
		n, err := readNetwork(ctx, h)
		if err != nil {
			return nil, err
		}
		iface, err := n.find(*a.Via)
		if err != nil {
			return nil, err
		}
		if !iface.CanProxy() {
			return nil, fmt.Errorf("a local proxy can't go out through %s: it takes Wi-Fi, Ethernet, cellular and Thunderbolt interfaces, not a VPN's or a bridge", n.describe(iface))
		}
		via = refOf(iface)
	}
	var out api.Proxy
	_, err := h.Edit(ctx, func(cfg *api.Config) error {
		k := slices.IndexFunc(cfg.Proxies, func(p api.Proxy) bool { return p.Port == a.Port })
		if k < 0 {
			if exits == 0 {
				return fmt.Errorf("no local proxy is on port %d: to add one, give via, upstream or scene", a.Port)
			}
			cfg.Proxies = append(cfg.Proxies, api.Proxy{Port: a.Port, Enabled: true})
			k = len(cfg.Proxies) - 1
		}
		p := &cfg.Proxies[k]
		switch {
		case exits == 0:
		case via.Name != "":
			p.Via, p.Upstream, p.Scene = via, "", ""
		case a.Upstream != nil && *a.Upstream != "":
			p.Via, p.Upstream, p.Scene = api.InterfaceRef{}, strings.TrimSpace(*a.Upstream), ""
		default:
			scene, err := findScene(cfg, *a.Scene)
			if err != nil {
				return err
			}
			p.Via, p.Upstream, p.Scene = api.InterfaceRef{}, "", scene.ID
		}
		if a.Enabled != nil {
			p.Enabled = *a.Enabled
		}
		if a.Shared != nil {
			p.Shared = *a.Shared
		}
		if a.ChangeIP != nil {
			p.ChangeIP = *a.ChangeIP
		}
		out = *p
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func removeProxy(ctx context.Context, h Host, a struct {
	Port int `json:"port"`
}) (any, error) {
	var out any
	_, err := h.Edit(ctx, func(cfg *api.Config) error {
		k := slices.IndexFunc(cfg.Proxies, func(p api.Proxy) bool { return p.Port == a.Port })
		if k < 0 {
			return fmt.Errorf("no local proxy is on port %d", a.Port)
		}
		out = map[string]any{"removed": cfg.Proxies[k]}
		cfg.Proxies = slices.Delete(cfg.Proxies, k, k+1)
		return nil
	})
	return out, err
}

// deviceIndex finds the device listed by the same address or range.
func deviceIndex(cfg *api.Config, address string) (int, error) {
	prefix, err := api.Device{Address: address}.Prefix()
	if err != nil {
		return 0, err
	}
	return slices.IndexFunc(cfg.Devices, func(d api.Device) bool {
		p, err := d.Prefix()
		return err == nil && p == prefix
	}), nil
}

func allowDevice(ctx context.Context, h Host, a struct {
	Address string  `json:"address"`
	Name    *string `json:"name"`
}) (any, error) {
	var out api.Device
	_, err := h.Edit(ctx, func(cfg *api.Config) error {
		k, err := deviceIndex(cfg, a.Address)
		if err != nil {
			return err
		}
		if k < 0 {
			cfg.Devices = append(cfg.Devices, api.Device{Address: strings.TrimSpace(a.Address)})
			k = len(cfg.Devices) - 1
		}
		if a.Name != nil {
			cfg.Devices[k].Name = strings.TrimSpace(*a.Name)
		}
		out = cfg.Devices[k]
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func removeDevice(ctx context.Context, h Host, a struct {
	Address string `json:"address"`
}) (any, error) {
	var out any
	_, err := h.Edit(ctx, func(cfg *api.Config) error {
		k, err := deviceIndex(cfg, a.Address)
		if err != nil {
			return err
		}
		if k < 0 {
			return fmt.Errorf("no device is listed by %s: get_config lists them", a.Address)
		}
		out = map[string]any{"removed": cfg.Devices[k]}
		cfg.Devices = slices.Delete(cfg.Devices, k, k+1)
		return nil
	})
	return out, err
}

// ---- public addresses -------------------------------------------------------

type ipChangeView struct {
	Interface   string  `json:"interface,omitempty"`
	Port        int     `json:"port,omitempty"`
	Device      string  `json:"device,omitempty"` // "hotspot" or "router"
	BeforeIPv4  string  `json:"beforeIPv4,omitempty"`
	BeforeIPv6  string  `json:"beforeIPv6,omitempty"`
	PublicIPv4  string  `json:"publicIPv4,omitempty"`
	PublicIPv6  string  `json:"publicIPv6,omitempty"`
	Changed     bool    `json:"changed"` // an address differs from the one of its family before
	IPv4Changed *bool   `json:"ipv4Changed,omitempty"`
	IPv6Changed *bool   `json:"ipv6Changed,omitempty"`
	Online      bool    `json:"online"` // the internet answers through it now
	Seconds     float64 `json:"seconds"`
	Standby     string  `json:"standby,omitempty"` // the interface that carried the Mac meanwhile
	Tries       int     `json:"tries,omitempty"`   // times a router gave its address up
	Problem     string  `json:"problem,omitempty"`
}

// A missing measurement leaves that family's comparison unknown.
func (v ipChangeView) compared() ipChangeView {
	if v.Online && v.BeforeIPv4 != "" && v.PublicIPv4 != "" {
		v.IPv4Changed = new(v.BeforeIPv4 != v.PublicIPv4)
		v.Changed = *v.IPv4Changed
	}
	if v.Online && v.BeforeIPv6 != "" && v.PublicIPv6 != "" {
		v.IPv6Changed = new(v.BeforeIPv6 != v.PublicIPv6)
		v.Changed = v.Changed || *v.IPv6Changed
	}
	return v
}

func viewRenewal(v ipChangeView, before, after api.Probe, took time.Duration) ipChangeView {
	v.BeforeIPv4, v.BeforeIPv6 = before.PublicIPv4, before.PublicIPv6
	v.PublicIPv4, v.PublicIPv6 = after.PublicIPv4, after.PublicIPv6
	v.Online, v.Problem = after.OK, after.Problem
	v.Seconds = float64(took.Round(100*time.Millisecond)) / float64(time.Second)
	return v.compared()
}

func changeIP(ctx context.Context, h Host, a struct {
	Interface string `json:"interface"`
	Port      int    `json:"port"`
}) (any, error) {
	switch {
	case a.Interface != "" && a.Port != 0:
		return nil, errors.New("give an interface or a port, not both")
	case a.Port != 0:
		c, err := h.ChangeProxyIP(ctx, a.Port)
		if err != nil {
			return nil, err
		}
		online := c.PublicIPv4 != "" || c.PublicIPv6 != ""
		return ipChangeView{Port: c.Port, BeforeIPv4: c.BeforeIPv4, BeforeIPv6: c.BeforeIPv6, PublicIPv4: c.PublicIPv4, PublicIPv6: c.PublicIPv6,
			Online: online, Seconds: c.Seconds}.compared(), nil
	}
	iface, err := interfaceNamed(ctx, h, a.Interface)
	if err != nil {
		return nil, err
	}
	// RMNet is identified by its hardware port, and an M2000 by its label.
	// Skip scraping /diagnostics/ before renewing a known hotspot.
	isHotspot := strings.EqualFold(iface.HardwarePort, api.HotspotRM520N) || hotspot.Model(iface.Label) != ""
	if !isHotspot {
		hs, _, err := gateway(ctx, h, iface.Name)
		if err != nil {
			return nil, err
		}
		isHotspot = hs != nil
	}
	if isHotspot {
		r, err := h.RenewHotspot(ctx, iface.Name)
		if err != nil {
			return nil, err
		}
		name := iface.Name
		if r.After.Interface != "" {
			name = r.After.Interface
		}
		return viewRenewal(ipChangeView{Interface: name, Device: "hotspot"}, r.Before, r.After, r.Took), nil
	}
	r, err := h.RenewRouter(ctx, iface.Name)
	if err != nil {
		return nil, err
	}
	return viewRenewal(ipChangeView{Interface: iface.Name, Device: "router", Standby: r.Standby, Tries: r.Tries}, r.Before, r.After, r.Took), nil
}
