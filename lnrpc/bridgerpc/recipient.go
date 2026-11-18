package bridgerpc

import (
	"encoding/json"
	"math"
	"strings"
)

// The ordinary pools are not the bridge's SHA-256 node. A wallet that
// opened to either of them still cannot pay a hold invoice with no route
// hint, and this node must not publish either key as its own.
const (
	eclairPoolKey = "034524e83896f38c3d8b5c14e0ef6f4092943c0cac5ef1267eac7273290b5c0324"
	xbtPoolKey    = "036497dba8574f56f3eba2a52b071e02063760bad94dd4246c26fb6026511be03a"
)

const (
	recipientChannelMin = 20_000
	recipientChannelMax = 16_777_215
	recipientAnchor     = 10_000
)

// RecipientOpeningSentence is what a payer is told when a channel toward
// that wallet is opening. The wallet adds its own line that the payment
// was not sent. This sentence must not say that itself.
const RecipientOpeningSentence = "no_route: A channel to this wallet is " +
	"opening. Try again after it confirms."

// RecipientState is what this node already has toward one wallet.
//
// OpenLocalSats is the most one open channel to that wallet can send after
// a 1% reserve. It is zero when the only channel points the other way.
type RecipientState struct {
	Enrolled          bool
	Connected         bool
	PendingOurChannel bool
	OpenLocalSats     int64
	ConfirmedSats     int64
}

// RecipientPlan says whether to open one channel toward an enrolled wallet.
//
// sats is the private channel size, with an empty push, when one should be
// opened. waiting means a channel this node already started is still
// confirming, so another must not be opened. Zero and not waiting means
// leave the quote's ordinary no-route answer as it is.
func RecipientPlan(dueMsat uint64, st RecipientState) (sats int64, waiting bool) {
	if !st.Enrolled || !st.Connected {
		return 0, false
	}
	if CoversDue(st.OpenLocalSats, dueMsat) {
		return 0, false
	}
	if st.PendingOurChannel {
		return 0, true
	}
	size, ok := channelSatsFor(dueMsat)
	if !ok {
		return 0, false
	}
	if st.ConfirmedSats < size+recipientAnchor {
		return 0, false
	}

	return size, false
}

// CoversDue reports whether spendable local sats can pay the invoice.
func CoversDue(spendable int64, dueMsat uint64) bool {
	due, ok := dueSats(dueMsat)

	return ok && spendable >= due
}

// SpendableLocal is local sats after a 1% reserve of the capacity.
func SpendableLocal(localSats, capacitySats int64) int64 {
	if localSats <= 0 {
		return 0
	}
	reserve := capacitySats / 100
	if reserve < 0 {
		reserve = 0
	}
	if localSats <= reserve {
		return 0
	}

	return localSats - reserve
}

// Enrolled reports whether dest is in the enrollment index. The index has
// no bridge code in it. A wallet that never enrolled is not a recipient
// this node opens toward.
func Enrolled(indexJSON []byte, dest string) bool {
	dest = strings.ToLower(strings.TrimSpace(dest))
	if !compressedKey(dest) {
		return false
	}
	var rows []struct {
		Pubkey string `json:"pubkey"`
	}
	if err := json.Unmarshal(indexJSON, &rows); err != nil {
		return false
	}
	for _, row := range rows {
		if strings.EqualFold(strings.TrimSpace(row.Pubkey), dest) {
			return true
		}
	}

	return false
}

// choosePublicPeer is the public Lightning name to publish, or empty.
//
// A clearnet host is preferred. An onion is used only when it is the only
// public host. A private address is not published, and neither pool key is.
func choosePublicPeer(identity string, uris []string) string {
	identity = strings.ToLower(strings.TrimSpace(identity))
	if !compressedKey(identity) || poolKey(identity) {
		return ""
	}
	prefix := identity + "@"
	var onion string
	for _, text := range uris {
		lower := strings.ToLower(strings.TrimSpace(text))
		host, ok := strings.CutPrefix(lower, prefix)
		if !ok || !publicHost(host) {
			continue
		}
		if onionHost(host) {
			if onion == "" {
				onion = prefix + host
			}
			continue
		}

		return prefix + host
	}

	return onion
}

func channelSatsFor(dueMsat uint64) (int64, bool) {
	due, ok := dueSats(dueMsat)
	if !ok || due > recipientChannelMax {
		return 0, false
	}
	withMin := due + recipientChannelMin
	withReserve := (due*100 + 98) / 99
	local := withMin
	if withReserve > local {
		local = withReserve
	}
	if local < recipientChannelMin || local > recipientChannelMax {
		return 0, false
	}

	return local, true
}

func dueSats(dueMsat uint64) (int64, bool) {
	if dueMsat == 0 || dueMsat > uint64(math.MaxInt64) {
		return 0, false
	}
	sats := int64((dueMsat + 999) / 1000)
	if sats <= 0 {
		return 0, false
	}

	return sats, true
}

func compressedKey(text string) bool {
	if len(text) != 66 || (text[0] != '0' || (text[1] != '2' && text[1] != '3')) {
		return false
	}
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		default:
			return false
		}
	}

	return true
}

func poolKey(peer string) bool {
	return peer == eclairPoolKey || peer == xbtPoolKey
}

func publicHost(host string) bool {
	if len(host) < 3 || len(host) > 300 || !allASCII(host) {
		return false
	}
	for i := 0; i < len(host); i++ {
		switch host[i] {
		case ' ', '\t', '\n', '\r', '/', '@', '?', '#', '\\':
			return false
		}
	}
	name, port, ok := splitHost(host)
	if !ok || !portOK(port) {
		return false
	}
	lower := strings.ToLower(name)
	if lower == "localhost" || strings.HasSuffix(lower, ".local") ||
		strings.HasSuffix(lower, ".localhost") {

		return false
	}
	if strings.HasPrefix(host, "[") || strings.Contains(name, ":") {
		return ipv6Public(lower)
	}
	if octets, is4 := ipv4Octets(lower); is4 {
		return ipv4Public(octets)
	}

	return true
}

func onionHost(host string) bool {
	name, _, ok := splitHost(host)

	return ok && strings.HasSuffix(strings.ToLower(name), ".onion")
}

func splitHost(host string) (string, string, bool) {
	if strings.HasPrefix(host, "[") {
		rest, ok := strings.CutPrefix(host, "[")
		if !ok {
			return "", "", false
		}
		addr, after, found := strings.Cut(rest, "]")
		if !found || addr == "" || !strings.HasPrefix(after, ":") {
			return "", "", false
		}
		for i := 0; i < len(addr); i++ {
			c := addr[i]
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') &&
				(c < 'A' || c > 'F') && c != ':' {

				return "", "", false
			}
		}

		return addr, strings.TrimPrefix(after, ":"), true
	}
	name, port, found := strings.Cut(host, ":")
	// The port is the last colon. An IPv6 address without brackets is
	// not a host this publishes.
	if strings.Contains(port, ":") {
		return "", "", false
	}
	if !found || name == "" || strings.Contains(name, ":") ||
		strings.HasPrefix(name, ".") || strings.HasPrefix(name, "-") ||
		strings.HasSuffix(name, ".") || strings.HasSuffix(name, "-") {

		return "", "", false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z', c == '.', c == '-':
		default:
			return "", "", false
		}
	}

	return name, port, true
}

func portOK(port string) bool {
	if port == "" || len(port) > 5 || (len(port) > 1 && port[0] == '0') {
		return false
	}
	n := 0
	for i := 0; i < len(port); i++ {
		if port[i] < '0' || port[i] > '9' {
			return false
		}
		n = n*10 + int(port[i]-'0')
	}

	return n > 0 && n <= 65535
}

func ipv4Octets(name string) ([4]int, bool) {
	var out [4]int
	part := 0
	n := -1
	digits := 0
	for i := 0; i <= len(name); i++ {
		if i == len(name) || name[i] == '.' {
			if digits == 0 || part >= 4 || n < 0 || n > 255 {
				return out, false
			}
			out[part] = n
			part++
			n = -1
			digits = 0
			continue
		}
		c := name[i]
		if c < '0' || c > '9' {
			return out, false
		}
		if digits == 1 && n == 0 {
			return out, false
		}
		if n < 0 {
			n = 0
		}
		n = n*10 + int(c-'0')
		digits++
		if digits > 3 {
			return out, false
		}
	}

	return out, part == 4
}

func ipv4Public(o [4]int) bool {
	a, b := o[0], o[1]
	if a == 0 || a == 10 || a == 127 {
		return false
	}
	if a == 169 && b == 254 {
		return false
	}
	if a == 172 && b >= 16 && b <= 31 {
		return false
	}
	if a == 192 && b == 168 {
		return false
	}
	if a == 100 && b >= 64 && b <= 127 {
		return false
	}

	return true
}

func ipv6Public(addr string) bool {
	switch addr {
	case "::", "::1", "0:0:0:0:0:0:0:1":
		return false
	}
	if strings.HasPrefix(addr, "fe8") || strings.HasPrefix(addr, "fe9") ||
		strings.HasPrefix(addr, "fea") || strings.HasPrefix(addr, "feb") ||
		strings.HasPrefix(addr, "fc") || strings.HasPrefix(addr, "fd") {

		return false
	}
	if mapped, ok := strings.CutPrefix(addr, "::ffff:"); ok {
		if octets, is4 := ipv4Octets(mapped); is4 {
			return ipv4Public(octets)
		}
	}

	return true
}

func allASCII(text string) bool {
	for i := 0; i < len(text); i++ {
		if text[i] > 127 {
			return false
		}
	}

	return true
}
