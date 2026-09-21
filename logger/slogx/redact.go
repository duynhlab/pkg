package slogx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// RedactPolicy is the one privacy boundary of the facade (RFC-0031 § Privacy,
// ADR-071). It runs before every sink — stdout and OTLP alike — on the
// message, on the record's attributes and on attributes bound with With,
// recursing through groups, maps, slices, structs, errors and LogValuers.
// Sampling is not a privacy control; this is.
//
// The policy can only be made STRICTER by a service: extra keys are added to
// the platform list, never substituted for it, and bounds can only be
// tightened. There is no way to turn redaction off.
//
// Two shapes are checked. Keys: an attribute (or map key, struct field, k=v
// pair inside a string) whose normalized key is on the deny list has its
// value replaced. Values: free text is scanned for credential shapes —
// Bearer/Basic authorization, JWTs, key=value pairs with a denied key, URL
// userinfo and Luhn-valid payment card numbers — and only the secret part is
// replaced so the line stays readable.
type RedactPolicy struct {
	// ExtraDenyKeys are added to the platform deny list. Matching is on the
	// normalized key: ASCII letters and digits kept and lower-cased,
	// fullwidth letters and digits folded to ASCII, every other rune ("-",
	// "_", ".", spaces, zero-width characters) dropped — so "Access-Token",
	// "access_token" and "accessToken" are one key. No other compatibility
	// normalization is applied. An entry ending in "*" matches any key that
	// CONTAINS the stem — "token*" catches "id_token" and "tokenHint" — which
	// is deliberately generous: a redacted counter is a nuisance, a leaked
	// credential is an incident. An entry that normalizes to nothing ("*",
	// "_*") is ignored rather than allowed to match every key.
	ExtraDenyKeys []string
	// MaxAttrs caps the leaf attributes on one record, groups and bound
	// attributes included; the rest are dropped and counted in
	// "_slogx.dropped". Only values below the platform cap (64) are
	// honoured.
	MaxAttrs int
	// MaxDepth caps nesting: the record's own attributes are depth 1, the
	// members of a top-level group depth 2, and a group that would open at
	// a depth beyond the cap (platform cap 4) is replaced by a marker.
	MaxDepth int
	// MaxValueLen caps a string value in bytes (platform cap 4096); longer
	// values are cut on a rune boundary and a 16-byte marker is appended.
	MaxValueLen int
	// MaxMessageLen caps the message (platform cap 1024). The message is a
	// display phrase; values belong in attributes.
	MaxMessageLen int
}

// Platform bounds. A service may tighten them, never widen them.
const (
	maxAttrs      = 64
	maxDepth      = 4
	maxValueLen   = 4096
	maxMessageLen = 1024
	// maxKeyLen caps an attribute key or group name.
	maxKeyLen = 128
	// scanSlack is how far past a bound the scanners still look, so a
	// secret that straddles the cut is redacted rather than half printed.
	// It exceeds every secret shape the scanners know (a JWT is bounded by
	// the value it sits in, not by this).
	scanSlack = 256
	// maxMarshalLen caps the JSON a struct may expand to before it is
	// walked; maxCollectionLen caps the elements of a slice or map that are
	// walked one by one.
	maxMarshalLen    = 64 * 1024
	maxCollectionLen = 4096
	// redacted is the marker written in place of a removed value;
	// depthMarker replaces a group that opens beyond MaxDepth, so an
	// operator does not read a nesting cut as "a secret was here".
	redacted    = "[REDACTED]"
	depthMarker = "[TRUNCATED: depth]"
	// droppedKey counts leaf attributes dropped by MaxAttrs. It is reserved
	// like the envelope keys: a user attribute with this key is dropped.
	// On a logger opened with WithGroup it renders inside that group, as
	// every attribute added at Handle time does.
	droppedKey = "_slogx.dropped"
	// truncatedMarker is appended to a value bound cut.
	truncatedMarker = "…(truncated)"
	// panicLimit bounds the text kept from a panicking method.
	panicLimit = 256
)

// DefaultRedactPolicy returns the platform policy: the ADR-071 deny list
// plus the keys RFC-0031 § Privacy names, at the platform bounds.
func DefaultRedactPolicy() RedactPolicy {
	return RedactPolicy{MaxAttrs: maxAttrs, MaxDepth: maxDepth, MaxValueLen: maxValueLen, MaxMessageLen: maxMessageLen}
}

// platformDenyKeys is the fleet deny list. Exact entries match the normalized
// key; "*" entries match any key containing the stem. Stems are used where a
// collision with a legitimate key is unlikely (nothing benign contains
// "password"); exact keys where a stem would swallow a pinned semantic
// convention ("body*" would deny http.request.body.size, "peer*"
// peer.service).
var platformDenyKeys = []string{
	// credentials and session material (ADR-071 minimum list, widened)
	"authorization", "proxyauthorization", "auth", "xauth", "basicauth", "authheader",
	"cookie*", "setcookie",
	"password*", "passwd*", "passphrase*",
	"token*", "bearer*", "jwt*", "credential*",
	"secret*", "apikey*", "privatekey*",
	"signature", "xsignature*", "xhubsignature*", "hmac",
	"otp", "otpcode", "onetimepassword", "onetimecode", "pin",
	"idempotencykey",
	// payment data
	"cvv*", "cvc*", "cardnumber", "pan", "cardpan", "ssn",
	// connection material (RFC-0031 § Privacy: connection strings)
	"dsn", "connstr*", "connectionstring*",
	"databaseurl", "databasedsn", "redisurl", "valkeyurl", "amqpurl", "brokerurl", "cacheurl",
	// forbidden request attributes (RFC-0031 § Canonical attributes)
	"clientaddress", "clientip", "remoteaddr", "remoteip",
	"peeraddress", "peeraddr", "peerip", "networkpeeraddress", "netpeerip", "netsockpeeraddr",
	"useragent", "httpuseragent", "useragentoriginal",
	"headers", "requestheaders", "responseheaders",
	"body", "requestbody", "responsebody", "reqbody", "respbody", "rawbody", "httpbody",
	"payload", "requestpayload", "responsepayload", "rawpayload",
}

// redactor is a compiled RedactPolicy. It is immutable after compile and safe
// for concurrent use.
type redactor struct {
	exact         map[string]struct{}
	stems         []string
	stemsB        [][]byte // the same stems, for the allocation-free ASCII path
	minStem       int      // shortest stem; a shorter key cannot contain one
	maxAttrs      int
	maxDepth      int
	maxValueLen   int
	maxMessageLen int
}

func compile(p RedactPolicy) *redactor {
	tighten := func(want, limit int) int {
		if want <= 0 || want > limit {
			return limit
		}
		return want
	}
	r := &redactor{
		exact:         map[string]struct{}{},
		maxAttrs:      tighten(p.MaxAttrs, maxAttrs),
		maxDepth:      tighten(p.MaxDepth, maxDepth),
		maxValueLen:   tighten(p.MaxValueLen, maxValueLen),
		maxMessageLen: tighten(p.MaxMessageLen, maxMessageLen),
	}
	for _, k := range append(append([]string{}, platformDenyKeys...), p.ExtraDenyKeys...) {
		stem, isStem := strings.CutSuffix(k, "*")
		n := normalizeKey(stem)
		if n == "" {
			continue // an empty stem would match every key
		}
		if isStem {
			r.stems = append(r.stems, n)
			r.stemsB = append(r.stemsB, []byte(n))
			if r.minStem == 0 || len(n) < r.minStem {
				r.minStem = len(n)
			}
			continue
		}
		r.exact[n] = struct{}{}
	}
	return r
}

// normalizeKey keeps letters and digits, lower-cased, with fullwidth forms
// folded to ASCII; everything else — the separators people vary on,
// zero-width characters — is dropped, so one spelling of a key cannot slip
// past another's deny entry. A key that is already lower-case ASCII letters
// and digits is returned as is, without allocating.
func normalizeKey(k string) string {
	plain := true
	for i := 0; i < len(k); i++ {
		c := k[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			plain = false
			break
		}
	}
	if plain {
		return k
	}
	var b strings.Builder
	b.Grow(len(k))
	for _, c := range k {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteRune(c)
		case c >= 'A' && c <= 'Z':
			b.WriteRune(c + ('a' - 'A'))
		case c >= '０' && c <= '９', c >= 'ａ' && c <= 'ｚ':
			b.WriteRune(c - ('０' - '0'))
		case c >= 'Ａ' && c <= 'Ｚ':
			b.WriteRune(c - ('Ａ' - 'a'))
		case unicode.IsLetter(c) || unicode.IsDigit(c):
			b.WriteRune(unicode.ToLower(c))
		}
	}
	return b.String()
}

func (r *redactor) denied(key string) bool {
	var buf [maxKeyLen]byte
	if n, ok := normalizeASCII(key, buf[:0]); ok {
		// string(n) in a map index does not allocate; bytes.Contains needs no
		// conversion at all — the common ASCII key costs nothing on the heap.
		if _, hit := r.exact[string(n)]; hit {
			return true
		}
		if len(n) < r.minStem {
			return false
		}
		for _, st := range r.stemsB {
			if bytes.Contains(n, st) {
				return true
			}
		}
		return false
	}
	n := normalizeKey(key)
	if _, ok := r.exact[n]; ok {
		return true
	}
	if len(n) < r.minStem {
		return false
	}
	for _, st := range r.stems {
		if strings.Contains(n, st) {
			return true
		}
	}
	return false
}

// normalizeASCII is normalizeKey for an ASCII key that fits the buffer: the
// same folding, written into dst. ok is false for a key with non-ASCII
// bytes or too many letters and digits — the caller falls back to
// normalizeKey.
func normalizeASCII(k string, dst []byte) (n []byte, ok bool) {
	for i := 0; i < len(k); i++ {
		c := k[i]
		switch {
		case c >= 0x80:
			return nil, false
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c >= 'A' && c <= 'Z':
			c += 'a' - 'A'
		default:
			continue
		}
		if len(dst) == cap(dst) {
			return nil, false
		}
		dst = append(dst, c)
	}
	return dst, true
}

// wholeLine reports a key whose value is a structured line rather than one
// token — an authorization header (scheme plus credential), a cookie header
// (name=value; name=value), a header dump — and must be removed to the end
// of the line, not to the first space or semicolon.
func wholeLine(key string) bool {
	switch n := normalizeKey(key); n {
	case "authorization", "proxyauthorization", "cookie", "setcookie",
		"headers", "requestheaders", "responseheaders":
		return true
	}
	return false
}

func isBlank(c byte) bool { return c == ' ' || c == '\t' }

// containerAt reports a value that opens a container at s[v]: {…}, […],
// map[…] or Go's &{…} pointer dump.
func containerAt(s string, v int) bool {
	if v >= len(s) {
		return false
	}
	switch s[v] {
	case '{', '[':
		return true
	case '&':
		return v+1 < len(s) && s[v+1] == '{'
	case 'm':
		return strings.HasPrefix(s[v:], "map[")
	}
	return false
}

// closeOf returns the index just past the bracket that closes the container
// opening at s[v] (quotes inside are skipped), or len(s) if it never closes.
func closeOf(s string, v int) int {
	i := v
	for i < len(s) && s[i] != '{' && s[i] != '[' {
		i++ // "map", "&"
	}
	depth := 0
	for ; i < len(s); i++ {
		switch s[i] {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return i + 1
			}
		case '"', '\'':
			q := s[i]
			for i++; i < len(s) && s[i] != q; i++ {
				if s[i] == '\\' {
					i++
				}
			}
		}
	}
	return len(s)
}

// Value scanners: the second half of the boundary. Keys are what a developer
// meant; values are what actually arrived, and a secret can arrive under any
// key — inside an error string, a URL, a header dump or a JSON body logged
// as text. Each pattern replaces only the secret part. Every scanner is
// guarded by a cheap byte-level prefilter so ordinary text pays no regex.
var (
	// "Authorization: Bearer eyJ…", "Basic dXNlcjpwYXNz"
	reBearer = regexp.MustCompile(`\b([Bb][Ee][Aa][Rr][Ee][Rr]|[Bb][Aa][Ss][Ii][Cc])\s+[A-Za-z0-9._~+/=-]{8,}`)
	// A JWT anywhere in the text.
	reJWT = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]*`)
	// URL userinfo: scheme://user:password@host. The user may be empty
	// (redis://:password@host); the password stops at "/" as RFC 3986
	// requires, so a port followed by an "@" later in the path is not one.
	reUserinfo = regexp.MustCompile(`://([^/\s:@]*):([^@\s/]+)@`)
)

// scan removes credential-shaped material from free text.
func (r *redactor) scan(s string) string {
	if hasAuthScheme(s) && reBearer.MatchString(s) {
		s = reBearer.ReplaceAllStringFunc(s, redactScheme)
	}
	if strings.Contains(s, "eyJ") && reJWT.MatchString(s) {
		s = reJWT.ReplaceAllString(s, redacted)
	}
	if strings.IndexByte(s, '=') >= 0 || strings.IndexByte(s, ':') >= 0 {
		s = r.scanKV(s)
	}
	if strings.Contains(s, "://") && strings.Contains(s, "@") && reUserinfo.MatchString(s) {
		s = reUserinfo.ReplaceAllString(s, "://$1:"+redacted+"@")
	}
	if hasDigitRun(s, 13) {
		s = scanPAN(s)
	}
	return s
}

// redactScheme rewrites one reBearer match. A credential after "Bearer" or
// "Basic" has digits, upper-case or symbols; a plain lower-case word after
// the same word ("basic validation failed") is prose and stays.
func redactScheme(m string) string {
	i := strings.IndexFunc(m, unicode.IsSpace)
	tok := strings.TrimLeftFunc(m[i:], unicode.IsSpace)
	for j := 0; j < len(tok); j++ {
		if tok[j] < 'a' || tok[j] > 'z' {
			return m[:i] + " " + redacted
		}
	}
	return m
}

// scanPAN replaces every 13–19 digit run (single spaces, dots, dashes or
// underscores allowed between digits) that stands on word boundaries and passes
// looksLikePAN. Runs are found with the same byte scan as hasDigitRun and
// searched by panIn, so a card number followed by an expiry or a CVV
// ("4111… 12/26", "4111… 123") is caught, while a UUID's digit groups —
// which never reach 13 digits on their own and fail Luhn together —
// survive. Hand-written for the same reason as scanKV: a regexp with a
// leading \b costs more per byte than the whole JSON encode.
func scanPAN(s string) string {
	var out []byte
	last := 0
	for i := 0; i < len(s); {
		for i < len(s) && !isDigit(s[i]) {
			i++
		}
		if i == len(s) {
			break
		}
		start, end := i, i
		for end < len(s) && extendsRun(s, end) {
			end++
		}
		i = end
		from, to, ok := panIn(s, start, end)
		if !ok {
			continue
		}
		out = append(out, s[last:from]...)
		out = append(out, redacted...)
		last = to
		i = to
	}
	if out == nil {
		return s
	}
	return string(append(out, s[last:]...))
}

// extendsRun reports whether s[end] continues a digit run: a digit, or a
// single separator with digits on both sides.
func extendsRun(s string, end int) bool {
	c := s[end]
	return isDigit(c) || (isPANSep(c) && end+1 < len(s) && isDigit(s[end+1]) && isDigit(s[end-1]))
}

// panIn looks for the first card number inside the digit run s[start:end].
// A candidate starts at the run or after any separator inside it, and must
// follow a non-word byte; it ends at the run or before any separator, and
// must precede a non-word byte (an underscore is a word character, as in a
// regexp \b). Every 13–19 digit candidate is Luhn-checked; the walk stops
// at 19 digits, so the cost is linear in the run.
func panIn(s string, start, end int) (from, to int, ok bool) {
	for from = start; from < end; from++ {
		if from > start && !isPANSep(s[from-1]) {
			continue
		}
		if from > 0 && isWordByte(s[from-1]) {
			continue
		}
		digits := 0
		for to = from; to <= end; to++ {
			if to < end && isDigit(s[to]) {
				digits++
				if digits > 19 {
					break
				}
				continue
			}
			// to sits on a separator or at the run's end: a group boundary
			if digits >= 13 && (to < end || end == len(s) || !isWordByte(s[end])) && looksLikePAN(s[from:to]) {
				return from, to, true
			}
		}
	}
	return 0, 0, false
}

func isDigit(c byte) bool  { return c >= '0' && c <= '9' }
func isPANSep(c byte) bool { return c == ' ' || c == '.' || c == '-' || c == '_' }
func isWordByte(c byte) bool {
	return isDigit(c) || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_'
}

// scanKV redacts the value of every key=value, key: value and "key":"value"
// pair whose key the deny list rejects — the same list, so "X-Auth-Token:",
// "POSTGRES_PASSWORD=" and {"clientSecret":…} inside a string are caught by
// the stems that catch them as attribute keys. A quoted value runs to its
// closing quote; an unquoted one to the next delimiter; an authorization
// header to the end of the line. Hand-written: one linear pass, no
// allocation unless something is replaced.
func (r *redactor) scanKV(s string) string {
	var out []byte
	last := 0
	for i := 0; i < len(s); i++ {
		if s[i] != '=' && s[i] != ':' {
			continue
		}
		// Walk back over "key", 'key' or key, with optional spaces.
		j := i
		for j > 0 && isBlank(s[j-1]) {
			j--
		}
		if j > 0 && (s[j-1] == '"' || s[j-1] == '\'') {
			j--
		}
		k := j
		for k > 0 && j-k < maxKeyLen && isKeyByte(s[k-1]) {
			k--
		}
		// A key longer than maxKeyLen is judged on its last maxKeyLen bytes:
		// the stems still match, and refusing to look would be a bypass.
		if j-k < 2 || !hasLetter(s[k:j]) || !r.denied(s[k:j]) {
			continue
		}
		// Value: optional blanks, then a quote, a container ({…}, […],
		// map[…], &{…}) or a bare token.
		v := i + 1
		for v < len(s) && isBlank(s[v]) {
			v++
		}
		var quote byte
		if v < len(s) && (s[v] == '"' || s[v] == '\'') {
			quote = s[v]
			v++
		}
		e := v
		switch {
		case quote == 0 && containerAt(s, v):
			// A denied key whose value is an object, array or %v dump is
			// removed whole to the matching close — its inner keys are not
			// individually on the list ("credentials": {"user":…,"pass":…}).
			e = closeOf(s, v)
		case quote != 0:
			for e < len(s) && s[e] != quote && s[e] != '\n' {
				if s[e] == '\\' {
					e++
				}
				e++
			}
			if e > len(s) {
				e = len(s)
			}
		case wholeLine(s[k:j]):
			for e < len(s) && s[e] != '\n' && s[e] != '\r' && s[e] != '}' {
				e++
			}
		default:
			for e < len(s) && !isValueEnd(s[e]) {
				e++
			}
		}
		if e == v {
			continue
		}
		if e > len(s) {
			e = len(s)
		}
		out = append(out, s[last:v]...)
		out = append(out, redacted...)
		last = e
		i = e - 1
	}
	if out == nil {
		return s
	}
	return string(append(out, s[last:]...))
}

// isKeyByte: letters, digits, the separators keys use, and every non-ASCII
// byte — so a fullwidth or zero-width-spliced key inside text reaches
// normalizeKey, which folds it the way it folds an attribute key.
func isKeyByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.' || c >= 0x80
}

// hasLetter reports an ASCII letter or any non-ASCII byte — a candidate key
// needs one; "10" in "10:30" or "2026-09-21" in a timestamp does not.
func hasLetter(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i] | 0x20; c >= 'a' && c <= 'z' || s[i] >= 0x80 {
			return true
		}
	}
	return false
}

func isValueEnd(c byte) bool {
	switch c {
	case '&', ' ', '\t', '\n', '\r', '"', '\'', ',', '}', ']', ';':
		return true
	}
	return false
}

// hasAuthScheme reports "bearer" or "basic" anywhere in s, ignoring case —
// the only strings reBearer can match. A plain byte loop: cheaper per byte
// than IndexAny's set, and most text has no "b" followed by those letters.
func hasAuthScheme(s string) bool {
	for i := 0; i+5 <= len(s); i++ {
		if s[i]|0x20 != 'b' {
			continue
		}
		if s[i+1]|0x20 == 'a' && s[i+2]|0x20 == 's' && s[i+3]|0x20 == 'i' && s[i+4]|0x20 == 'c' {
			return true
		}
		if i+6 <= len(s) && s[i+1]|0x20 == 'e' && s[i+2]|0x20 == 'a' && s[i+3]|0x20 == 'r' && s[i+4]|0x20 == 'e' && s[i+5]|0x20 == 'r' {
			return true
		}
	}
	return false
}

// hasDigitRun reports n digits joined by at most one separator (space,
// dot, dash, underscore) each — the only strings scanPAN can match.
func hasDigitRun(s string, n int) bool {
	run, sep := 0, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isDigit(c) {
			run++
			sep = false
			if run >= n {
				return true
			}
			continue
		}
		if run > 0 && !sep && isPANSep(c) {
			sep = true
			continue
		}
		run, sep = 0, false
	}
	return false
}

// text applies the value rules: scan for embedded credentials within the
// bound plus a slack, repair UTF-8, cut on a rune boundary. Scanning a
// window rather than the whole value keeps a megabyte error string from
// buying a megabyte of regex time for a 4 KiB line.
func (r *redactor) text(s string, limit int) string {
	if len(s) > limit+scanSlack {
		s = s[:limit+scanSlack]
	}
	return bound(r.scan(s), limit)
}

func (r *redactor) str(s string) string { return r.text(s, r.maxValueLen) }

func bound(s string, limit int) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + truncatedMarker
}

// looksLikePAN reports whether s is 13–19 digits (single spaces, dots or
// dashes allowed between digits) that pass the Luhn check
// AND start with an issuer range a payment card can have: 3 (Amex, JCB,
// Diners), 4 (Visa), 5 or 2 (Mastercard), 6 (Discover, UnionPay, Maestro).
// The IIN gate cuts the one-in-ten false positive Luhn alone has on random
// numeric ids; a 13–19 digit id starting with 1, 7, 8, 9 or 0 is never
// touched.
func looksLikePAN(s string) bool {
	digits := make([]byte, 0, 19)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			if len(digits) == 19 {
				return false
			}
			digits = append(digits, c-'0')
		case isPANSep(c):
			continue
		default:
			return false
		}
	}
	if len(digits) < 13 {
		return false
	}
	switch digits[0] {
	case 2, 3, 4, 5, 6:
	default:
		return false
	}
	sum := 0
	double := false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i])
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}

// budget tracks the leaf-attribute allowance across one record, its groups
// and its bound attributes.
type budget struct {
	left    int
	dropped int
}

func (b *budget) take() bool {
	if b.left <= 0 {
		b.dropped++
		return false
	}
	b.left--
	return true
}

// attrs redacts an attribute list against a shared budget. Attributes beyond
// the budget are dropped — never emitted raw — and counted.
func (r *redactor) attrs(in []slog.Attr, b *budget, depth int) []slog.Attr {
	out := make([]slog.Attr, 0, len(in))
	for _, a := range in {
		if red, ok := r.attr(a, b, depth); ok {
			out = append(out, red)
		}
	}
	return out
}

// attr redacts one attribute: the key is checked first, the value only if
// the key survives. The deny check runs on the key BEFORE the value is
// resolved or rendered, so a denied key never executes a LogValuer, Stringer
// or Error method whose output would be discarded anyway. A key that is
// itself credential-shaped, or reserved, drops the attribute.
func (r *redactor) attr(a slog.Attr, b *budget, depth int) (slog.Attr, bool) {
	if a.Key == "" {
		if v := a.Value.Resolve(); v.Kind() == slog.KindGroup {
			// slog inlines an empty-key group into its parent, so its
			// members sit at THIS depth, not one deeper.
			out := r.attrs(v.Group(), b, depth)
			if len(out) == 0 {
				return slog.Attr{}, false
			}
			return slog.Attr{Value: slog.GroupValue(out...)}, true
		}
		b.dropped++ // slog would print an empty key; we do not
		return slog.Attr{}, false
	}
	if a.Key == droppedKey || r.scan(a.Key) != a.Key {
		b.dropped++
		return slog.Attr{}, false
	}
	if r.denied(a.Key) {
		if !b.take() {
			return slog.Attr{}, false
		}
		return slog.String(bound(a.Key, maxKeyLen), redacted), true
	}
	return r.value(bound(a.Key, maxKeyLen), a.Value, b, depth)
}

// value redacts a value under an already-accepted key.
func (r *redactor) value(key string, v slog.Value, b *budget, depth int) (slog.Attr, bool) {
	if v.Kind() == slog.KindLogValuer && isTypedNil(v.Any()) {
		// Resolve would recover the nil-receiver panic and render slog's
		// own stack trace, build-machine paths included.
		if !b.take() {
			return slog.Attr{}, false
		}
		return slog.Attr{Key: key, Value: slog.AnyValue(nil)}, true
	}
	v = v.Resolve()
	switch v.Kind() {
	case slog.KindGroup:
		return r.group(key, v.Group(), b, depth)
	case slog.KindAny:
		return r.walkAny(key, v.Any(), b, depth)
	}
	if !b.take() {
		return slog.Attr{}, false
	}
	return slog.Attr{Key: key, Value: r.scalar(v)}, true
}

// scalar applies the value rules to one leaf: strings are scanned and
// bounded, integers and whole floats large enough to be a card number are
// Luhn-checked, everything else (bool, duration, time) passes through.
func (r *redactor) scalar(v slog.Value) slog.Value {
	switch v.Kind() {
	case slog.KindString:
		return slog.StringValue(r.str(v.String()))
	case slog.KindInt64:
		if n := v.Int64(); n >= 1e12 && looksLikePAN(strconv.FormatInt(n, 10)) {
			return slog.StringValue(redacted)
		}
	case slog.KindUint64:
		if n := v.Uint64(); n >= 1e12 && looksLikePAN(strconv.FormatUint(n, 10)) {
			return slog.StringValue(redacted)
		}
	case slog.KindFloat64:
		if f := v.Float64(); f >= 1e12 && f < 1<<63 && f == float64(int64(f)) && looksLikePAN(strconv.FormatInt(int64(f), 10)) {
			return slog.StringValue(redacted)
		}
	}
	return v
}

// group redacts the members of a group one level deeper; a group that would
// open beyond the depth cap is replaced by the depth marker, and an empty
// group is dropped as slog itself drops empty groups.
func (r *redactor) group(key string, members []slog.Attr, b *budget, depth int) (slog.Attr, bool) {
	if depth >= r.maxDepth {
		if !b.take() {
			return slog.Attr{}, false
		}
		return slog.String(key, depthMarker), true
	}
	out := r.attrs(members, b, depth+1)
	if len(out) == 0 {
		return slog.Attr{}, false
	}
	return slog.Attr{Key: key, Value: slog.GroupValue(out...)}, true
}

// walkAny handles the values slog would otherwise hand to encoding/json
// whole — where a deny-listed field inside a struct or map would sail past a
// key-based check. Errors, Stringers and URLs become their scanned, bounded
// text; slices and string-keyed maps are walked element by element so a
// LogValuer, error or Stringer element is honoured; structs round-trip
// through JSON (which honours json:"-") and are walked as groups so every
// nested key is checked. A LogValuer inside a struct FIELD is marshalled
// like any field — hide such a field with json:"-" or log it as its own
// attribute. A method that panics is caught here, as slog's own handler
// would have caught it, and the value is replaced by a marker that names
// the type and carries only scanned text — a log line must never take the
// process down, and a panic message must never print what redaction hides.
func (r *redactor) walkAny(key string, x any, b *budget, depth int) (red slog.Attr, ok bool) {
	var taken, decided bool
	take := func() bool {
		if !decided {
			decided, taken = true, b.take()
		}
		return taken
	}
	defer func() {
		if p := recover(); p != nil {
			if !take() {
				red, ok = slog.Attr{}, false
				return
			}
			red, ok = slog.String(key, r.panicText(p)), true
		}
	}()
	if x == nil || isTypedNil(x) {
		if !take() {
			return slog.Attr{}, false
		}
		return slog.Attr{Key: key, Value: slog.AnyValue(nil)}, true
	}
	switch t := x.(type) {
	case *url.URL:
		if !take() {
			return slog.Attr{}, false
		}
		return slog.String(key, r.str(t.Redacted())), true
	case error:
		if !take() {
			return slog.Attr{}, false
		}
		return slog.String(key, r.str(t.Error())), true
	case fmt.Stringer:
		if !take() {
			return slog.Attr{}, false
		}
		return slog.String(key, r.str(t.String())), true
	case []byte:
		if !take() {
			return slog.Attr{}, false
		}
		return slog.String(key, r.str(string(t))), true
	}
	rv := reflect.ValueOf(x)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			// json.RawMessage and every other named byte slice are text,
			// not 4096 small integers that spell the secret in decimal.
			if !take() {
				return slog.Attr{}, false
			}
			if rv.Kind() == reflect.Array {
				rv = reflect.ValueOf(bytesOfArray(rv))
			}
			return slog.String(key, r.str(string(rv.Bytes()))), true
		}
		return r.slice(key, rv, b, depth)
	case reflect.Map:
		if rv.Type().Key().Kind() == reflect.String {
			return r.stringMap(key, rv, b, depth)
		}
	}
	if _, marshals := x.(json.Marshaler); !marshals {
		if rv.Kind() == reflect.Pointer && !rv.IsNil() && rv.Elem().Kind() == reflect.Struct {
			rv = rv.Elem()
		}
		if rv.Kind() == reflect.Struct {
			return r.structFields(key, rv, b, depth)
		}
	}
	// Anything else (a type that marshals itself, a map with non-string
	// keys, a channel, a func): round-trip through JSON so its keys are
	// visible to the deny list. A value that cannot be marshalled, or that
	// is larger than the platform will walk, is REPLACED — never rendered
	// with %v, which would print the very fields the author hid.
	raw, err := json.Marshal(x)
	if err != nil {
		if !take() {
			return slog.Attr{}, false
		}
		return slog.String(key, redacted+" (unmarshalable "+rv.Type().String()+")"), true
	}
	if len(raw) > maxMarshalLen {
		if !take() {
			return slog.Attr{}, false
		}
		return slog.String(key, redacted+" (value larger than "+strconv.Itoa(maxMarshalLen)+" bytes)"), true
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		if !take() {
			return slog.Attr{}, false
		}
		return slog.String(key, redacted+" (undecodable "+rv.Type().String()+")"), true
	}
	return r.value(key, fromJSON(generic), b, depth)
}

// slice walks a slice or array as an indexed group ("0", "1", …), each
// element redacted as its own value. Elements past the budget are counted
// as dropped without being visited.
func (r *redactor) slice(key string, rv reflect.Value, b *budget, depth int) (slog.Attr, bool) {
	n := rv.Len()
	if a, done := r.collectionGuard(key, n, b, depth); done {
		return a, a.Key != "" || a.Value.Kind() != slog.KindAny
	}
	members := make([]slog.Attr, 0, min(n, b.left))
	for i := 0; i < n; i++ {
		if b.left <= 0 {
			b.dropped += n - i
			break
		}
		if m, ok := r.value(strconv.Itoa(i), slog.AnyValue(rv.Index(i).Interface()), b, depth+1); ok {
			members = append(members, m)
		}
	}
	if len(members) == 0 {
		return slog.Attr{}, false
	}
	return slog.Attr{Key: key, Value: slog.GroupValue(members...)}, true
}

// structFields walks a struct's exported fields as a group, named as
// encoding/json would name them (the json tag, else the field name; "-"
// skips; anonymous struct fields are flattened), each value re-entering the
// key check and the value rules — so a LogValuer, error or Stringer FIELD
// is honoured and a string field is bounded without marshalling the whole
// struct first.
func (r *redactor) structFields(key string, rv reflect.Value, b *budget, depth int) (slog.Attr, bool) {
	if a, done := r.collectionGuard(key, rv.NumField(), b, depth); done {
		return a, a.Key != "" || a.Value.Kind() != slog.KindAny
	}
	members := r.fieldAttrs(rv, b, depth+1, nil)
	if len(members) == 0 {
		return slog.Attr{}, false
	}
	return slog.Attr{Key: key, Value: slog.GroupValue(members...)}, true
}

func (r *redactor) fieldAttrs(rv reflect.Value, b *budget, depth int, members []slog.Attr) []slog.Attr {
	t := rv.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		fv := rv.Field(i)
		if f.Anonymous && name == "" {
			// encoding/json flattens an embedded struct even when its type
			// name is unexported; a nil embedded pointer contributes nothing.
			for fv.Kind() == reflect.Pointer {
				if fv.IsNil() {
					break
				}
				fv = fv.Elem()
			}
			if fv.Kind() == reflect.Struct {
				members = r.fieldAttrs(fv, b, depth, members)
			}
			if fv.Kind() == reflect.Struct || fv.Kind() == reflect.Pointer {
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		if b.left <= 0 {
			b.dropped++
			continue
		}
		if m, ok := r.attr(slog.Attr{Key: name, Value: slog.AnyValue(fv.Interface())}, b, depth); ok {
			members = append(members, m)
		}
	}
	return members
}

// collectionGuard replaces a collection that is too large to walk or too
// deep to open. done reports that the caller should return a as is (a zero
// Attr when the budget is spent).
func (r *redactor) collectionGuard(key string, n int, b *budget, depth int) (a slog.Attr, done bool) {
	switch {
	case n > maxCollectionLen:
		if !b.take() {
			return slog.Attr{}, true
		}
		return slog.String(key, redacted+" (collection of "+strconv.Itoa(n)+" elements)"), true
	case depth >= r.maxDepth:
		if !b.take() {
			return slog.Attr{}, true
		}
		return slog.String(key, depthMarker), true
	}
	return slog.Attr{}, false
}

// stringMap walks a string-keyed map as a group with sorted keys, each
// entry going through the key check like any attribute.
func (r *redactor) stringMap(key string, rv reflect.Value, b *budget, depth int) (slog.Attr, bool) {
	n := rv.Len()
	if a, done := r.collectionGuard(key, n, b, depth); done {
		return a, a.Key != "" || a.Value.Kind() != slog.KindAny
	}
	keys := rv.MapKeys()
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	members := make([]slog.Attr, 0, min(n, b.left))
	for i, k := range keys {
		if b.left <= 0 {
			b.dropped += n - i
			break
		}
		a := slog.Attr{Key: k.String(), Value: slog.AnyValue(rv.MapIndex(k).Interface())}
		if m, ok := r.attr(a, b, depth+1); ok {
			members = append(members, m)
		}
	}
	if len(members) == 0 {
		return slog.Attr{}, false
	}
	return slog.Attr{Key: key, Value: slog.GroupValue(members...)}, true
}

// bytesOfArray copies a byte array out of a possibly unaddressable value.
func bytesOfArray(rv reflect.Value) []byte {
	out := make([]byte, rv.Len())
	for i := range out {
		out[i] = byte(rv.Index(i).Uint())
	}
	return out
}

// panicText renders a recovered panic without rendering the panic VALUE: the
// type is named, and only a string or error payload contributes text — after
// the same scan every value gets. A %v of an arbitrary value would print
// the fields its author hid.
func (r *redactor) panicText(p any) string {
	msg := "!PANIC (" + reflect.TypeOf(p).String() + ")"
	switch v := p.(type) {
	case string:
		msg += ": " + r.text(v, panicLimit)
	case error:
		msg += ": " + r.text(safeError(v), panicLimit)
	}
	return bound(msg, panicLimit+64)
}

// safeError calls Error() on a value whose Error() may itself panic.
func safeError(err error) (s string) {
	defer func() {
		if recover() != nil {
			s = "error whose Error() panics"
		}
	}()
	return err.Error()
}

// isTypedNil reports a nil pointer, map, slice, func or chan hiding behind a
// non-nil interface — calling Error() or String() on it would panic.
func isTypedNil(x any) bool {
	rv := reflect.ValueOf(x)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
		return rv.IsNil()
	}
	return false
}

// fromJSON converts decoded JSON into slog values: objects become groups
// (keys sorted, so output is stable) so their keys are checked, arrays become
// indexed groups, numbers keep their integer precision, scalars pass through.
func fromJSON(x any) slog.Value {
	switch t := x.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		attrs := make([]slog.Attr, 0, len(t))
		for _, k := range keys {
			attrs = append(attrs, slog.Attr{Key: k, Value: fromJSON(t[k])})
		}
		return slog.GroupValue(attrs...)
	case []any:
		attrs := make([]slog.Attr, 0, len(t))
		for i, v := range t {
			attrs = append(attrs, slog.Attr{Key: strconv.Itoa(i), Value: fromJSON(v)})
		}
		return slog.GroupValue(attrs...)
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return slog.Int64Value(i)
		}
		if f, err := t.Float64(); err == nil {
			return slog.Float64Value(f)
		}
		return slog.StringValue(t.String())
	case string:
		return slog.StringValue(t)
	case bool:
		return slog.BoolValue(t)
	default:
		return slog.AnyValue(nil)
	}
}

// redactHandler applies the policy before the sinks. It is the outermost
// handler: below it the level gate and trace correlation (traceHandler)
// add the envelope's own attributes, which never enter the redactor and
// never spend its budget — a record at the attribute cap keeps its
// trace_id, and an all-digit span id is not a card number. Attributes bound
// with WithAttrs are redacted at bind time and count against the same budget
// as the record's own; groups opened with WithGroup count toward the depth
// cap.
type redactHandler struct {
	r       *redactor
	next    slog.Handler
	bound   int // leaf attributes already bound through WithAttrs
	dropped int // leaf attributes dropped at bind time, reported on every record
	depth   int // groups already opened through WithGroup
}

func (h redactHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h redactHandler) Handle(ctx context.Context, rec slog.Record) error {
	in := make([]slog.Attr, 0, rec.NumAttrs())
	rec.Attrs(func(a slog.Attr) bool { in = append(in, a); return true })
	b := &budget{left: h.r.maxAttrs - h.bound, dropped: h.dropped}
	out := slog.NewRecord(rec.Time, rec.Level, h.r.text(rec.Message, h.r.maxMessageLen), rec.PC)
	out.AddAttrs(h.r.attrs(in, b, h.depth+1)...)
	if b.dropped > 0 {
		out.AddAttrs(slog.Int(droppedKey, b.dropped))
	}
	return h.next.Handle(ctx, out)
}

// WithAttrs redacts the bound attributes too — the case that is easy to
// forget: a logger bound once with a secret would otherwise leak it on every
// line for the rest of the process.
func (h redactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	b := &budget{left: h.r.maxAttrs - h.bound}
	red := h.r.attrs(attrs, b, h.depth+1)
	return redactHandler{
		r:       h.r,
		next:    h.next.WithAttrs(red),
		bound:   h.r.maxAttrs - b.left,
		dropped: h.dropped + b.dropped,
		depth:   h.depth,
	}
}

// WithGroup opens a group; a credential-shaped or over-long name is replaced.
func (h redactHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h // the slog.Handler contract: an empty name is a no-op
	}
	if h.r.scan(name) != name {
		name = redacted
	}
	return redactHandler{r: h.r, next: h.next.WithGroup(bound(name, maxKeyLen)), bound: h.bound, dropped: h.dropped, depth: h.depth + 1}
}
