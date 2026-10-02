package core

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"mtssh/config"
	"mtssh/logger"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// HostKeyDecision is the result of asking the user about an unknown host
type HostKeyDecision int

const (
	HostKeyAccept HostKeyDecision = iota
	HostKeyReject
)

// HostKeyPrompt is called when a host key is not yet known.
// It should block until the user makes a decision.
type HostKeyPrompt func(host, keyType, fingerprint string) HostKeyDecision

var khMu sync.Mutex

// KnownHostsPath returns the location of MTSSH's own known_hosts file.
func KnownHostsPath() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "known_hosts"), nil
}

// unknownHostKeyAlgorithms is offered to hosts without a stored key: the
// x/crypto defaults without certificate algorithms (MTSSH verifies plain
// host keys; a server with a host certificate would otherwise present it
// and fail without a prompt) and without DSA.
var unknownHostKeyAlgorithms = []string{
	ssh.KeyAlgoECDSA256,
	ssh.KeyAlgoECDSA384,
	ssh.KeyAlgoECDSA521,
	ssh.KeyAlgoRSASHA256,
	ssh.KeyAlgoRSASHA512,
	ssh.KeyAlgoRSA,
	ssh.KeyAlgoED25519,
}

// Errors from BuildHostKeyCallback. ssh.Dial wraps them, so use errors.Is.
var (
	// ErrHostKeyMismatch: the host presented a key other than the stored one.
	ErrHostKeyMismatch = errors.New("HOST KEY MISMATCH")
	// ErrHostKeyRejected: the user did not trust an unknown host's key.
	ErrHostKeyRejected = errors.New("host key rejected by user")

	errUnknownHost = errors.New("unknown host")
)

// BuildHostKeyCallback returns an ssh.HostKeyCallback that:
//  1. Accepts known hosts from ~/.mtssh/known_hosts
//  2. Calls prompt for unknown hosts and appends accepted keys
//  3. Rejects changed host keys (MITM protection)
//
// khMu is not held while prompting: the answer may take long or never come
// (window closed), and other connections must not wait for it.
//
// Build one callback per connection: it remembers the key it accepted, and
// later key exchanges on the connection (re-keying) must present the same
// key. They are not checked against the file again, so removing the entry
// in the Known Hosts manager does not pop up a prompt mid-session.
func BuildHostKeyCallback(prompt HostKeyPrompt) ssh.HostKeyCallback {
	return hostKeyCallback(prompt, "")
}

// hostKeyCallback is BuildHostKeyCallback. If legacyAddr is not "", it is
// the address as older versions stored it (host as entered, before names
// were lower-cased and stripped of a trailing dot); it is looked up too when
// the host is otherwise unknown. loadKnownHosts folds the names of such
// entries, so they are found under the normalized name as well, whatever
// case the host is entered in now.
func hostKeyCallback(prompt HostKeyPrompt, legacyAddr string) ssh.HostKeyCallback {
	var mu sync.Mutex
	accepted := map[string][]byte{} // host → key accepted on this connection
	check := hostKeyChecker(prompt, legacyAddr)
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		mu.Lock()
		pinned, ok := accepted[hostname]
		mu.Unlock()
		if ok {
			if bytes.Equal(pinned, key.Marshal()) {
				return nil
			}
			return fmt.Errorf("%w for %s — the key changed during the session", ErrHostKeyMismatch, hostname)
		}
		if err := check(hostname, remote, key); err != nil {
			return err
		}
		mu.Lock()
		accepted[hostname] = key.Marshal()
		mu.Unlock()
		return nil
	}
}

func hostKeyChecker(prompt HostKeyPrompt, legacyAddr string) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		path, err := KnownHostsPath()
		if err != nil {
			return err
		}
		lookup := func() error {
			err := checkHostKey(path, hostname, remote, key)
			if errors.Is(err, errUnknownHost) && legacyAddr != "" && legacyAddr != hostname {
				// An entry an older version stored: the same key is known
				// (nothing is appended), another key is a mismatch.
				if lerr := checkHostKey(path, legacyAddr, remote, key); !errors.Is(lerr, errUnknownHost) {
					return lerr
				}
			}
			return err
		}
		khMu.Lock()
		err = lookup()
		khMu.Unlock()
		if !errors.Is(err, errUnknownHost) {
			return err // nil (known and matching), mismatch or I/O error
		}
		// Don't ask about a key that could not be stored safely.
		if err := checkStorableAddr(hostname); err != nil {
			return err
		}

		// Unknown host — ask user
		if prompt(hostname, key.Type(), ssh.FingerprintSHA256(key)) != HostKeyAccept {
			return fmt.Errorf("%w for %s", ErrHostKeyRejected, hostname)
		}

		khMu.Lock()
		defer khMu.Unlock()
		// Another connection may have stored a key for this host meanwhile.
		if err := lookup(); !errors.Is(err, errUnknownHost) {
			return err
		}
		if err := appendKnownHost(path, hostname, key); err != nil {
			return fmt.Errorf("could not save host key: %w", err)
		}
		return nil
	}
}

// checkHostKey returns nil if key is stored for hostname, errUnknownHost if
// the host has no stored key, and ErrHostKeyMismatch if it has other keys.
// khMu must be held.
func checkHostKey(path, hostname string, remote net.Addr, key ssh.PublicKey) error {
	// Ensure the file exists so knownhosts.New doesn't fail
	if err := ensureFile(path); err != nil {
		return err
	}
	db, err := loadKnownHosts(path)
	if err != nil {
		return err
	}
	err = db.check(hostname, remote, key)
	if err == nil {
		return nil
	}
	var keyErr *knownhosts.KeyError
	if !errors.As(err, &keyErr) {
		return fmt.Errorf("known_hosts: %w", err) // e.g. a revoked key
	}
	want := plainKeys(keyErr.Want, db.caLines)
	if len(want) == 0 {
		// A line that could not be read may hold this host's key: don't
		// offer to trust whatever key the host presents now.
		if n := db.skippedLine(hostname); n > 0 {
			return fmt.Errorf("known_hosts line %d is invalid and names %s — fix or remove it", n, hostname)
		}
		// No key stored, or only @cert-authority lines: MTSSH does not
		// use host certificates, so the host is unknown.
		return errUnknownHost
	}
	return fmt.Errorf("%w for %s — possible MITM attack\nExpected: %s\nGot: %s",
		ErrHostKeyMismatch, hostname,
		ssh.FingerprintSHA256(want[0].Key), ssh.FingerprintSHA256(key))
}

// khDB is the parsed known_hosts file.
type khDB struct {
	check   ssh.HostKeyCallback
	caLines map[int]bool // numbers of @cert-authority lines
	skipped []khSkipped  // lines knownhosts could not parse
}

// khSkipped is a known_hosts line that was ignored as invalid.
type khSkipped struct {
	line  int
	hosts string // its host field; "" if hashed or missing
}

// maxSkippedKnownHosts bounds the invalid lines loadKnownHosts skips; each
// one costs another parse of the whole file.
const maxSkippedKnownHosts = 100

// maxKnownHostsSize bounds the known_hosts file MTSSH reads: every
// connection parses it, and the Known Hosts manager lists it. Real files are
// far smaller; a larger one is refused instead of read into memory.
const maxKnownHostsSize = 16 << 20

// readKnownHostsFile reads the known_hosts file, at most maxKnownHostsSize
// bytes.
func readKnownHostsFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxKnownHostsSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxKnownHostsSize {
		return nil, fmt.Errorf("%s is larger than %d MB", path, maxKnownHostsSize>>20)
	}
	return data, nil
}

// ReadKnownHosts returns the content of the known_hosts file for the Known
// Hosts manager, or nothing if there is none yet. It has the size limit
// connections have, so the manager lists what they read.
func ReadKnownHosts() ([]byte, error) {
	khMu.Lock()
	defer khMu.Unlock()

	path, err := KnownHostsPath()
	if err != nil {
		return nil, err
	}
	data, err := readKnownHostsFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return data, err
}

// loadKnownHosts parses the known_hosts file with knownhosts. A line it
// cannot parse (e.g. edited by hand) is skipped and logged instead of making
// every connection fail, as OpenSSH does. It fails closed, though, if the
// line is an @revoked line, and checkHostKey does not report a host that a
// skipped line names as unknown. khMu must be held.
//
// knownhosts only reads files, so it parses a copy of the data read here.
// The file is read once: a change made meanwhile by another process cannot
// put the line numbers knownhosts reports out of step with lines. In the
// copy, host names are folded like the name looked up (see khFoldLine).
func loadKnownHosts(path string) (*khDB, error) {
	data, err := readKnownHostsFile(path)
	if err != nil {
		return nil, fmt.Errorf("known_hosts: %w", err)
	}
	lines := strings.Split(string(data), "\n")
	folded := make([]string, len(lines)) // what knownhosts parses
	for i, l := range lines {
		folded[i] = khFoldLine(l)
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".known_hosts-*") // mode 0600
	if err != nil {
		return nil, fmt.Errorf("known_hosts: %w", err)
	}
	tmp := f.Name()
	f.Close()
	defer os.Remove(tmp)

	db := &khDB{caLines: map[int]bool{}}
	seen := map[int]bool{}
	for {
		if err := os.WriteFile(tmp, []byte(strings.Join(folded, "\n")), 0600); err != nil {
			return nil, fmt.Errorf("known_hosts: %w", err)
		}
		check, err := knownhosts.New(tmp)
		if err == nil {
			db.check = check
			break
		}
		// "knownhosts: <file>:<N>: <reason>"; anything else (e.g. a line
		// longer than 64 KB) cannot be skipped.
		n, reason, ok := khErrorLine(err, tmp)
		if !ok || n < 1 || n > len(lines) || seen[n] {
			return nil, fmt.Errorf("known_hosts: %w", err)
		}
		if len(db.skipped) >= maxSkippedKnownHosts {
			return nil, fmt.Errorf("known_hosts: more than %d invalid lines — fix the file", maxSkippedKnownHosts)
		}
		seen[n] = true
		fields := khFields(lines[n-1])
		if len(fields) > 0 && fields[0] == "@revoked" {
			return nil, fmt.Errorf("known_hosts line %d: invalid @revoked entry — fix or remove it", n)
		}
		logger.Error("known_hosts", fmt.Sprintf("ignoring invalid known_hosts line %d: %s", n, reason))
		db.skipped = append(db.skipped, khSkipped{line: n, hosts: khHostField(fields)})
		// Blank, so line numbers stay the same.
		lines[n-1], folded[n-1] = "", ""
	}
	for i, l := range lines {
		if f := khFields(l); len(f) > 0 && f[0] == "@cert-authority" {
			db.caLines[i+1] = true
		}
	}
	return db, nil
}

// khFoldLine returns a known_hosts line with its host patterns folded the
// way knownHostName folds the host looked up (see khFoldPattern).
// knownhosts compares host names byte for byte, so an entry an older version
// stored as "SRV01" or "[LOCALHOST]:2222" would otherwise not match
// "srv01" or "localhost", and a changed key would get a trust prompt instead
// of a mismatch. OpenSSH ignores case too. Only the host field changes, so
// the line keeps its marker, key and number. Comments, hashed hosts (|1|…)
// and lines with an unknown marker stay as they are.
func khFoldLine(line string) string {
	start := khSkipBlanks(line, 0)
	if start == len(line) || line[start] == '#' {
		return line
	}
	end := khWordEnd(line, start)
	if w := line[start:end]; w == "@cert-authority" || w == "@revoked" {
		start = khSkipBlanks(line, end)
		end = khWordEnd(line, start)
	}
	hosts := line[start:end]
	if hosts == "" || hosts[0] == '|' || hosts[0] == '@' {
		return line
	}
	patterns := strings.Split(hosts, ",")
	for i, p := range patterns {
		patterns[i] = khFoldPattern(p)
	}
	return line[:start] + strings.Join(patterns, ",") + line[end:]
}

// khSkipBlanks returns the index of the first byte from i on that is not a
// space or tab, the separators knownhosts uses.
func khSkipBlanks(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return i
}

// khWordEnd returns the index of the first space or tab from i on.
func khWordEnd(s string, i int) int {
	for i < len(s) && s[i] != ' ' && s[i] != '\t' {
		i++
	}
	return i
}

// khFoldPattern lower-cases the host of one host pattern and, if it has no
// wildcard, strips a trailing dot, like knownHostName. A negation stays a
// negation, the port stays, and a pattern knownhosts cannot read keeps its
// form, so the line fails the same way.
func khFoldPattern(p string) string {
	neg := ""
	if strings.HasPrefix(p, "!") {
		neg, p = "!", p[1:]
	}
	p = lowerASCII(p)
	// Split as knownhosts does: "[host]:port", "host:port" or a host.
	host, port, err := net.SplitHostPort(p)
	switch {
	case err == nil && strings.HasPrefix(p, "["):
		return neg + "[" + khTrimDot(host) + "]:" + port
	case err == nil:
		return neg + khTrimDot(host) + ":" + port
	case strings.HasPrefix(p, "["):
		return neg + p // invalid
	}
	return neg + khTrimDot(p)
}

// khTrimDot strips a trailing dot from a host name. A wildcard pattern keeps
// it: "*." would become "*", which matches every host. So does ".", which
// would become an empty pattern.
func khTrimDot(host string) string {
	if len(host) < 2 || strings.ContainsAny(host, "*?") {
		return host
	}
	return strings.TrimSuffix(host, ".")
}

// lowerASCII lower-cases A–Z only, as OpenSSH does. strings.ToLower would
// also map some other characters to ASCII letters (the Kelvin sign to "k").
func lowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// khErrorLine parses an error of knownhosts.New(file) that names a line.
func khErrorLine(err error, file string) (line int, reason string, ok bool) {
	rest, ok := strings.CutPrefix(err.Error(), "knownhosts: "+file+":")
	if !ok {
		return 0, "", false
	}
	num, reason, ok := strings.Cut(rest, ": ")
	if !ok {
		return 0, "", false
	}
	line, err = strconv.Atoi(num)
	return line, reason, err == nil
}

// khFields splits a known_hosts line into fields the way knownhosts does:
// separated by spaces and tabs.
func khFields(line string) []string {
	return strings.FieldsFunc(line, func(r rune) bool { return r == ' ' || r == '\t' })
}

// khHostField returns the host patterns of a line split by khFields, or ""
// for a hashed host.
func khHostField(fields []string) string {
	if len(fields) > 0 && strings.HasPrefix(fields[0], "@") {
		fields = fields[1:] // marker
	}
	if len(fields) == 0 || strings.HasPrefix(fields[0], "|") {
		return ""
	}
	return fields[0]
}

// skippedLine returns the number of a skipped line whose host field lists
// addr's host (or [host]:port), ignoring case, or 0. Hashed lines cannot be
// matched; OpenSSH skips invalid ones too.
func (db *khDB) skippedLine(addr string) int {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	host = knownHostName(host)
	for _, s := range db.skipped {
		for _, p := range strings.Split(s.hosts, ",") {
			h := p
			if strings.HasPrefix(p, "[") {
				bh, bp, err := net.SplitHostPort(p)
				if err != nil || bp != port {
					continue
				}
				h = bh
			}
			if knownHostName(h) == host {
				return s.line
			}
		}
	}
	return 0
}

// plainKeys drops keys that come from @cert-authority lines.
func plainKeys(keys []knownhosts.KnownKey, caLines map[int]bool) []knownhosts.KnownKey {
	var out []knownhosts.KnownKey
	for _, k := range keys {
		if !caLines[k.Line] {
			out = append(out, k)
		}
	}
	return out
}

// probeKey is a key that is never stored; checking it makes knownhosts
// report every key stored for a host.
var probeKey, _ = ssh.NewPublicKey(ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)).Public())

// knownHostKeyAlgorithms returns the host key algorithms to offer a host
// with stored keys, or nil if the host is unknown: first the ones matching
// the stored keys, then the rest of unknownHostKeyAlgorithms, as OpenSSH
// does. The client's order wins, so a server that has a stored key presents
// it. Otherwise a server that adds a key of a type the client prefers (e.g.
// ECDSA next to a stored Ed25519 key) would be reported as a host key
// mismatch. A server that has none of the stored types (another server, or
// a MITM) presents another key, and the host key check reports the change.
// Offering only the stored types would fail with "no common algorithm"
// instead, without the warning, and reconnects would keep trying.
func knownHostKeyAlgorithms(addr string) []string {
	khMu.Lock()
	defer khMu.Unlock()

	path, err := KnownHostsPath()
	if err != nil {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	db, err := loadKnownHosts(path)
	if err != nil {
		return nil
	}
	var keyErr *knownhosts.KeyError
	if err := db.check(addr, &net.TCPAddr{}, probeKey); !errors.As(err, &keyErr) {
		return nil
	}

	var algos []string
	seen := map[string]bool{}
	add := func(names ...string) {
		for _, a := range names {
			if !seen[a] {
				seen[a] = true
				algos = append(algos, a)
			}
		}
	}
	for _, k := range plainKeys(keyErr.Want, db.caLines) {
		if typ := k.Key.Type(); typ == ssh.KeyAlgoRSA {
			// An RSA key can be used with any of the RSA signature algorithms.
			add(ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA)
		} else {
			add(typ)
		}
	}
	if len(algos) == 0 {
		return nil // only @cert-authority lines: unknown
	}
	add(unknownHostKeyAlgorithms...)
	return algos
}

// RemoveKnownHost deletes every line of the known_hosts file equal to line
// (ignoring surrounding whitespace). An empty line clears the whole file.
func RemoveKnownHost(line string) error {
	khMu.Lock()
	defer khMu.Unlock()

	path, err := KnownHostsPath()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var out []string
	if line != "" {
		want := strings.TrimSpace(line)
		for _, l := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
			if strings.TrimSpace(l) != want {
				out = append(out, l)
			}
		}
	}
	content := strings.Join(out, "\n")
	if content != "" {
		content += "\n"
	}
	return writeFileAtomic(path, []byte(content))
}

// writeFileAtomic replaces path via a temp file in the same directory, so a
// crash cannot leave a truncated or empty file behind: the data is synced
// before the rename, the directory after it.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*") // mode 0600
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op once renamed
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	syncDir(dir)
	return nil
}

// syncDir persists a rename in dir. Best effort: Windows cannot sync a
// directory, and some file systems don't support it.
func syncDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

func ensureFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	return f.Close()
}

// checkStorableAddr accepts host:port only if the host passes CheckHost as
// it is (no spaces or brackets to strip).
func checkStorableAddr(hostname string) error {
	host, _, err := net.SplitHostPort(hostname)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidHost, err)
	}
	if h, err := validHost(host); err != nil {
		return err
	} else if h != host {
		return fmt.Errorf("%w: %q", ErrInvalidHost, host)
	}
	return nil
}

// appendKnownHost is only called for hosts checkHostKey reported as unknown,
// with khMu held, so the entry cannot already exist. hostname is host:port;
// only a host that passes CheckHost is stored, so the line cannot become a
// pattern or list that matches other hosts.
func appendKnownHost(path, hostname string, key ssh.PublicKey) error {
	if err := checkStorableAddr(hostname); err != nil {
		return err
	}
	line := knownhosts.Line([]string{hostname}, key)
	field, _, _ := strings.Cut(line, " ")
	if field != knownhosts.Normalize(hostname) || strings.ContainsAny(field, ",*?!") ||
		strings.ContainsFunc(field, unicode.IsSpace) {
		return fmt.Errorf("%w: %q cannot be stored in known_hosts", ErrInvalidHost, hostname)
	}
	line += "\n"

	f, err := os.OpenFile(path, os.O_APPEND|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	// A file edited by hand may lack the final newline; appending would
	// then merge our entry into its last line.
	if fi, err := f.Stat(); err == nil && fi.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, fi.Size()-1); err == nil && last[0] != '\n' {
			line = "\n" + line
		}
	}
	if _, err := f.WriteString(line); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
