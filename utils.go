package fpoc

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	igncfg "github.com/coreos/ignition/v2/config/v3_4"
	igntyp "github.com/coreos/ignition/v2/config/v3_4/types"
	"github.com/coreos/vcontext/report"
	"github.com/go-viper/mapstructure/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/hashicorp/go-hclog"
)

// PluginNetwork extends servers.Network with subnet selection support.
// When SubnetID is set, the plugin pre-creates a Neutron port on the
// specified subnet and passes the port ID to Nova instead of the network UUID.
type PluginNetwork struct {
	UUID     string `json:"uuid,omitempty"`
	Port     string `json:"port,omitempty"`
	FixedIP  string `json:"fixed_ip,omitempty"`
	Tag      string `json:"tag,omitempty"`
	SubnetID string `json:"subnet_id,omitempty"`
}

// ExtCreateOpts is servers.CreateOpts plus the fields gophercloud has no
// equivalent for: image/flavor lookup by name, a description, a key name,
// scheduler hints, and networks that can name a subnet.
type ExtCreateOpts struct {
	servers.CreateOpts

	// fields absent in gophercloud
	Description string `json:"description,omitempty"`
	KeyName     string `json:"key_name,omitempty"`

	// search for imageRef by name each time
	ImageName string `json:"image_name,omitempty"`

	// search for imageRef by metadata each time
	ImageRefFromMetadata string `json:"image_ref_from_metadata,omitempty"`

	// search for flavorRef by name each time
	FlavorName string `json:"flavor_name,omitempty"`

	// annotation overrides
	Networks       []PluginNetwork            `json:"networks,omitempty"`
	SecurityGroups []string                   `json:"security_groups,omitempty"`
	UserData       string                     `json:"user_data,omitempty"`
	SchedulerHints *servers.SchedulerHintOpts `json:"scheduler_hints,omitempty"`
}

// ToServerCreateMap for extended opts
func (opts ExtCreateOpts) ToServerCreateMap() (map[string]interface{}, error) {
	if opts.Networks != nil {
		networks := make([]servers.Network, len(opts.Networks))
		for i, net := range opts.Networks {
			networks[i] = servers.Network{
				UUID:    net.UUID,
				Port:    net.Port,
				FixedIP: net.FixedIP,
				Tag:     net.Tag,
			}
		}
		opts.CreateOpts.Networks = networks
	}

	if opts.SecurityGroups != nil {
		opts.CreateOpts.SecurityGroups = opts.SecurityGroups
	}

	if opts.UserData != "" {
		opts.CreateOpts.UserData = []byte(opts.UserData)
	}

	ob, err := opts.CreateOpts.ToServerCreateMap()
	if err != nil {
		return nil, err
	}

	b := map[string]any{}
	if opts.Description != "" {
		b["description"] = opts.Description
	}
	if opts.KeyName != "" {
		b["key_name"] = opts.KeyName
	}

	sob, ok := ob["server"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("unexpected server create map: %q is %T, want map[string]any", "server", ob["server"])
	}
	maps.Copy(sob, b)

	return ob, nil
}

// Address is one entry from a Nova server's addresses map.
type Address struct {
	Version int    `json:"version"`
	Address string `json:"addr"`
	MACAddr string `json:"OS-EXT-IPS-MAC:mac_addr,omitempty"`
	Type    string `json:"OS-EXT-IPS:type,omitempty"`
}

func extractAddresses(srv *servers.Server) (map[string][]Address, error) {
	ret := make(map[string][]Address, len(srv.Addresses))

	for net, isv := range srv.Addresses {
		// srv.Addresses is decoded straight from Nova's JSON, so a
		// malformed or unexpected response must not panic the plugin.
		ism, ok := isv.([]any)
		if !ok {
			return nil, fmt.Errorf("unexpected addresses for network %q: %T, want a list", net, isv)
		}
		items := make([]Address, 0, len(ism))

		for _, iv := range ism {
			var out Address

			cfg := &mapstructure.DecoderConfig{
				Metadata: nil,
				Result:   &out,
				TagName:  "json",
			}
			decoder, _ := mapstructure.NewDecoder(cfg)
			err := decoder.Decode(iv)
			if err != nil {
				return nil, err
			}

			items = append(items, out)
		}

		ret[net] = items
	}

	return ret, nil
}

// selectConnectAddresses picks the addresses ConnectInfo reports as an
// instance's internal and external endpoints, out of every address
// extractAddresses found across all of its networks.
//
// The split matters: GitLab Runner's connector dials InternalAddr unless
// runners.autoscaler.connector_config.use_external_addr is set, which
// defaults to false. Reporting a floating address as the internal one would
// send a runner that shares the tenant network with its workers out through
// the NAT gateway and back — paying egress for a link it already has, and
// failing outright on clouds that don't support hairpin NAT. So a fixed
// (tenant-internal) address becomes internal and a floating (externally
// routable) one becomes external; Nova's OS-EXT-IPS:type field distinguishes
// them. When the instance only has one kind, both endpoints fall back to it,
// since an address the runner might not reach still beats no address at all.
//
// Selection within each class has to be deterministic: netAddrs is a map,
// and Go deliberately randomizes map iteration order, so picking "whichever
// address the range loop visits last" (the original approach) could return a
// different network's address on every call. Ordering is IPv4 before IPv6,
// then network name and address as a final, arbitrary-but-stable tie-break.
//
// Both return values are "" if netAddrs has no addresses.
func selectConnectAddresses(netAddrs map[string][]Address) (internal, external string) {
	type candidate struct {
		network string
		addr    Address
	}

	var candidates []candidate

	for net, addrs := range netAddrs {
		for _, addr := range addrs {
			candidates = append(candidates, candidate{net, addr})
		}
	}

	slices.SortFunc(candidates, func(a, b candidate) int {
		return cmp.Or(
			cmp.Compare(addressRank(a.addr), addressRank(b.addr)),
			cmp.Compare(a.network, b.network),
			cmp.Compare(a.addr.Address, b.addr.Address),
		)
	})

	for _, c := range candidates {
		if isFloating(c.addr) {
			if external == "" {
				external = c.addr.Address
			}

			continue
		}

		if internal == "" {
			internal = c.addr.Address
		}
	}

	// Only one kind of address available — report it as both.
	if internal == "" {
		internal = external
	}

	if external == "" {
		external = internal
	}

	return internal, external
}

// isFloating reports whether a is an externally routable floating address
// rather than a tenant-internal fixed one.
func isFloating(a Address) bool { return a.Type == "floating" }

// addressRank orders addresses within one class (floating or fixed) for
// selectConnectAddresses; lower sorts first. IPv4 is preferred over IPv6
// because it is the more universally routable of the two in the runner's
// own network.
func addressRank(a Address) int {
	if a.Version == 4 {
		return 0
	}

	return 1
}

var (
	initFinishedRe   = regexp.MustCompile(`^.*Cloud-init\ v\.\ \S+\ finished\ at.*$`)
	initSSHHostKeyRe = regexp.MustCompile(`^SSH\ host\ key:\ \S+:\S+\ (\S+)$`)
	initLoginRe      = regexp.MustCompile(`^\S+\ login:\ .*$`)
)

// IsCloudInitFinished reports whether a console log shows cloud-init has
// completed, which is how a cloud-init instance signals it is ready.
func IsCloudInitFinished(log string) bool {
	lines := strings.Split(log, "\n")

	for _, line := range lines {
		if initFinishedRe.MatchString(line) {
			return true
		}
	}
	return false
}

// IsIgnitionFinished reports whether a console log shows an Ignition-based
// instance (Flatcar, Fedora CoreOS) has finished booting. Flatcar prints no
// completion marker, so this looks for the SSH host key banner followed by a
// login prompt.
func IsIgnitionFinished(log string) bool {
	lines := strings.Split(log, "\n")

	// Flatcar do not have any meaningful line,
	// so instead we first check that there ssh host key message
	// followed with login prompt
	searchKeys := true

	for _, line := range lines {
		if searchKeys {
			if initSSHHostKeyRe.MatchString(line) {
				searchKeys = false
			}
		} else {
			if initLoginRe.MatchString(line) {
				return true
			}
		}
	}
	return false
}

// InsertSSHKeyIgn authorizes pubKey for username in the spec's Ignition
// user_data, parsing and extending any config the operator supplied rather
// than replacing it.
//
// log receives the non-fatal findings of parsing that config — deprecations
// and warnings. Ignition reports those separately from err (a fatal report
// is what produces err in the first place), so discarding them, as this used
// to, meant an operator whose user_data used a deprecated field got no
// signal at all until the instance failed to boot. May be nil.
func InsertSSHKeyIgn(spec *ExtCreateOpts, username, pubKey string, log hclog.Logger) error {
	var cfg igntyp.Config
	var err error

	if spec.UserData != "" {
		var rpt report.Report

		cfg, rpt, err = igncfg.ParseCompatibleVersion([]byte(spec.UserData))
		if err != nil {
			return fmt.Errorf("failed to parse ignition: %w", err)
		}

		if log != nil {
			for _, entry := range rpt.Entries {
				log.Warn("Ignition user_data", "kind", entry.Kind.String(), "entry", entry.String())
			}
		}
	}

	if cfg.Ignition.Version == "" {
		cfg.Ignition.Version = igntyp.MaxVersion.String()
	}

	var user *igntyp.PasswdUser
	if cfg.Passwd.Users == nil {
		cfg.Passwd.Users = make([]igntyp.PasswdUser, 0)
	}

	for idx, lu := range cfg.Passwd.Users {
		if lu.Name == username {
			user = &cfg.Passwd.Users[idx]
			break
		}
	}
	if user == nil {
		cfg.Passwd.Users = append(cfg.Passwd.Users, igntyp.PasswdUser{Name: username})
		user = &cfg.Passwd.Users[len(cfg.Passwd.Users)-1]
	}

	if user.SSHAuthorizedKeys == nil {
		user.SSHAuthorizedKeys = make([]igntyp.SSHAuthorizedKey, 0)
	}

	user.SSHAuthorizedKeys = append(user.SSHAuthorizedKeys, igntyp.SSHAuthorizedKey(pubKey))

	buf, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("failed to marshal ignition: %w", err)
	}

	spec.UserData = string(buf)
	return nil
}
