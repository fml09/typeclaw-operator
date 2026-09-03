// Package netblocks owns the address ranges carved out of an allow-all egress
// block so that "the public Internet" never quietly includes the cluster, the
// nodes, the metadata service, or the control plane.
//
// It exists as its own package because two boundaries need the same carve-outs
// and neither can import the other: internal/resources renders the Runtime's
// PublicWeb egress, and internal/desktop renders the Desktop Gateway's. A
// second copy of these lists would drift, and a drifted carve-out is a hole
// nobody notices until something reaches an address it should not.
package netblocks

// PublicWebV4Except enumerates the RFC special-use and private IPv4 ranges
// carved out of 0.0.0.0/0. 100.64.0.0/10 is in the list because it is CGNAT
// space, which is also where tailnet addresses live: traffic to a tailnet peer
// belongs inside the WireGuard tunnel, never as plain routed egress.
var PublicWebV4Except = []string{
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"169.254.0.0/16",
	"100.64.0.0/10",
	"127.0.0.0/8",
	"0.0.0.0/8",
	"192.0.0.0/24",
	"198.18.0.0/15",
	"224.0.0.0/4",
}

// PublicWebV6Except carves loopback, unique-local, and link-local ranges out
// of ::/0.
var PublicWebV6Except = []string{
	"::1/128",
	"fc00::/7",
	"fe80::/10",
}
