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

// ExtCreateOpts extended version of servers.CreateOpts
// nolint:revive
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

// selectConnectAddress picks one address to connect to out of every address
// extractAddresses found across all of an instance's networks.
//
// This has to be deterministic: netAddrs is a map, and Go deliberately
// randomizes map iteration order, so picking "whichever address the range
// loop visits last" (the previous approach) could return a different
// network's address on every call — including one the runner can't
// actually reach.
//
// Preference order: a floating (externally routable) address over a fixed
// (tenant-internal) one — Nova's OS-EXT-IPS:type field distinguishes them —
// then IPv4 over IPv6, then network name and address as a final,
// arbitrary-but-stable tie-break. Returns "" if netAddrs has no addresses.
func selectConnectAddress(netAddrs map[string][]Address) string {
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

	if len(candidates) == 0 {
		return ""
	}

	slices.SortFunc(candidates, func(a, b candidate) int {
		return cmp.Or(
			cmp.Compare(addressRank(a.addr), addressRank(b.addr)),
			cmp.Compare(a.network, b.network),
			cmp.Compare(a.addr.Address, b.addr.Address),
		)
	})

	return candidates[0].addr.Address
}

// addressRank scores an address for selectConnectAddress; lower sorts
// first. A floating address always outranks a fixed one regardless of IP
// version, since reachability matters more than protocol preference.
func addressRank(a Address) int {
	rank := 0
	if a.Type != "floating" {
		rank += 2
	}
	if a.Version != 4 {
		rank++
	}
	return rank
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
func InsertSSHKeyIgn(spec *ExtCreateOpts, username, pubKey string) error {
	var cfg igntyp.Config
	var err error

	if spec.UserData != "" {
		var rpt report.Report

		cfg, rpt, err = igncfg.ParseCompatibleVersion([]byte(spec.UserData))
		if err != nil {
			return fmt.Errorf("failed to parse ignition: %w", err)
		}

		_ = rpt
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
