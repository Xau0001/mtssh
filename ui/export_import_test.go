package ui

import (
	"errors"
	"fmt"
	"mtssh/config"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestParseImport(t *testing.T) {
	existing := []config.Session{{ID: "prod", Label: "prod-db", Host: "db.example", Port: 22, User: "admin", Password: "secret", AutoConnect: true}}
	data := `[
		{"id": "new", "label": "web", "host": " [::1] ", "port": 0, "user": "u", "auto_connect": true},
		{"id": "prod", "label": "prod-db", "host": "evil.example", "port": 22, "user": "admin", "password": ""},
		{"id": "dup", "label": "a", "host": "h1", "port": 22, "user": "u"},
		{"id": "dup", "label": "b", "host": "h2", "port": 22, "user": "u"},
		{"label": "bad port", "host": "h", "port": 70000, "user": "u"},
		{"label": "space", "host": "1.2.3.4 evil", "port": 22, "user": "u"},
		{"label": "newline", "host": "h\nx", "port": 22, "user": "u"},
		{"label": "no user", "host": "h", "port": 22}
	]`
	res, err := parseImport([]byte(data), existing)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.added) != 2 || len(res.duplicates) != 1 || len(res.invalid) != 5 || res.nInvalid != 5 {
		t.Fatalf("added %d, duplicates %d, invalid %d (%v)", len(res.added), len(res.duplicates), res.nInvalid, res.invalid)
	}
	web := res.added[0]
	if web.Host != "::1" || web.Port != 22 || web.AutoConnect {
		t.Fatalf("imported session not normalized: %+v", web)
	}

	// Overwriting with another host must not carry the stored password
	// over, nor connect there on its own at the next start.
	merged := overwriteSessions(append([]config.Session(nil), existing...), res.duplicates)
	if merged[0].Host != "evil.example" || merged[0].Password != "" {
		t.Fatalf("password kept for a changed host: %+v", merged[0])
	}
	if merged[0].AutoConnect {
		t.Fatal("auto-connect kept for a changed host")
	}
	d := describeOverwrite(existing, res.duplicates)
	for _, want := range []string{"evil.example", "password removed", "auto-connect turned off"} {
		if !strings.Contains(d, want) {
			t.Fatalf("overwrite description lacks %q: %q", want, d)
		}
	}

	// Same account: an export without passwords keeps the stored one.
	same := []config.Session{{ID: "prod", Label: "renamed", Host: "DB.example", Port: 22, User: "admin"}}
	merged = overwriteSessions(append([]config.Session(nil), existing...), same)
	if merged[0].Password != "secret" || merged[0].Label != "renamed" || !merged[0].AutoConnect {
		t.Fatalf("same account: %+v", merged[0])
	}
}

// sessEntryList returns a JSON list of n empty entries.
func sessEntryList(n int) []byte {
	return []byte("[" + strings.TrimSuffix(strings.Repeat("{},", n), ",") + "]")
}

func TestParseImportLimits(t *testing.T) {
	start := time.Now()
	res, err := parseImport(sessEntryList(sessMaxImportEntries), nil)
	if err != nil {
		t.Fatal(err)
	}
	msg := res.summary(0)
	if res.nInvalid != sessMaxImportEntries || len(res.invalid) != sessMaxListed ||
		len(msg) > 4096 || !strings.Contains(msg, "…and 9980 more") {
		t.Fatalf("%d invalid, %d listed, message of %d bytes: %s", res.nInvalid, len(res.invalid), len(msg), msg)
	}
	for _, n := range []int{sessMaxImportEntries + 1, 20000} {
		_, err := parseImport(sessEntryList(n), nil)
		if err == nil || len(err.Error()) > 200 {
			t.Fatalf("%d entries: err %v", n, err)
		}
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("took %v", d)
	}

	// No sessions, as before.
	for _, data := range []string{"null", " [ ] ", "[]\n", "\nnull\n"} {
		res, err := parseImport([]byte(data), nil)
		if err != nil || len(res.added)+len(res.duplicates)+res.nInvalid != 0 {
			t.Errorf("%q: %+v, %v", data, res, err)
		}
	}
	// Not a (single, complete) list of sessions: the whole file is refused.
	for _, data := range []string{
		"", "{}", `"x"`, "[{}", "[{},]", "[{} {}]", "[] []", "[]]", "[] x", "null null",
		"[1]", `[{"port": "22"}]`, `[{}, null, {"label": 5}]`,
	} {
		if _, err := parseImport([]byte(data), nil); err == nil {
			t.Errorf("%q accepted", data)
		}
	}
}

func TestParseImportMessagesBounded(t *testing.T) {
	long := strings.Repeat("é", 200) // valid, but longer than shown
	data := fmt.Sprintf(`[
		{"label": %q, "host": "h", "user": "u"},
		{"id": "a", "label": %q, "host": "h", "user": "u"},
		{"id": "a", "label": %q, "host": "h", "user": "u"},
		{"label": %q, "host": "*", "user": "u"}
	]`, strings.Repeat("x", 100000), long, long, "a"+sessRLO+"b")
	res, err := parseImport([]byte(data), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.added) != 1 || res.nInvalid != 3 {
		t.Fatalf("added %d, invalid %v", len(res.added), res.invalid)
	}
	for _, m := range res.invalid {
		if n := utf8.RuneCountInString(m); n > 200 || strings.Contains(m, sessRLO) {
			t.Errorf("message of %d characters: %q", n, m)
		}
	}
	if !strings.Contains(res.invalid[1], "duplicate id") || !strings.Contains(res.invalid[1], "…") {
		t.Errorf("duplicate id message: %q", res.invalid[1])
	}
	// logger.Clean shows the override as a Go escape.
	if !strings.Contains(res.invalid[2], `a\`+`u202eb`) {
		t.Errorf("label not escaped: %q", res.invalid[2])
	}
}

func TestOverwriteDetails(t *testing.T) {
	existing := []config.Session{
		{ID: "same", Label: "same", Host: "h", Port: 22, User: "u", Password: "p", AutoConnect: true},
		{ID: "key", Label: "key", Host: "h", Port: 22, User: "u", AutoConnect: true},
		{ID: "usekey", Label: "usekey", Host: "h", Port: 22, User: "u", KeyPath: "~/.ssh/a", AutoConnect: true},
		{ID: "host", Label: "host", Host: "h", Port: 22, User: "u", Password: "p", AutoConnect: true},
		{ID: "label", Label: "old" + sessRLO + "name", Host: "::1", Port: 22, User: "u"},
	}
	dups := []config.Session{
		{ID: "same", Label: "same", Host: "H", Port: 22, User: "u"},
		{ID: "key", Label: "key", Host: "h", Port: 22, User: "u", UseKey: true, KeyPath: "~/.ssh/other"},
		{ID: "usekey", Label: "usekey", Host: "h", Port: 22, User: "u", UseKey: true, KeyPath: "~/.ssh/a"},
		{ID: "host", Label: "host", Host: "evil.example", Port: 22, User: "u"},
		{ID: "label", Label: "new", Host: "::1", Port: 2222, User: "u"},
	}
	merged := overwriteSessions(append([]config.Session(nil), existing...), dups)
	for i, want := range []bool{true, false, false, false, false} {
		if merged[i].AutoConnect != want {
			t.Errorf("%s: auto-connect %v, want %v", merged[i].ID, merged[i].AutoConnect, want)
		}
	}
	if merged[0].Password != "p" || merged[3].Password != "" {
		t.Errorf("passwords: %q, %q", merged[0].Password, merged[3].Password)
	}

	d := describeOverwrite(existing, dups)
	blocks := strings.Split(d, "• ")[1:]
	if len(blocks) != len(dups) {
		t.Fatalf("description: %q", d)
	}
	for i, c := range []struct{ want, notWant []string }{
		{notWant: []string{"account", "password", "auto-connect", "key"}},
		{want: []string{"use SSH key: off → on", "key path: none → ~/.ssh/other", "auto-connect turned off"}, notWant: []string{"account"}},
		{want: []string{"use SSH key: off → on", "auto-connect turned off"}, notWant: []string{"key path"}},
		{want: []string{"account: u@h:22 → u@evil.example:22", "stored password removed", "auto-connect turned off"}},
		{want: []string{"old" + sessBad + "name\n", "label: old" + sessBad + "name → new", "account: u@[::1]:22 → u@[::1]:2222"},
			notWant: []string{sessRLO, "auto-connect", "password"}},
	} {
		for _, w := range c.want {
			if !strings.Contains(blocks[i], w) {
				t.Errorf("%s: lacks %q: %q", dups[i].ID, w, blocks[i])
			}
		}
		for _, w := range c.notWant {
			if strings.Contains(blocks[i], w) {
				t.Errorf("%s: contains %q: %q", dups[i].ID, w, blocks[i])
			}
		}
	}

	// Many sessions with long labels: a bounded list.
	existing, dups = nil, nil
	for i := 0; i < 25; i++ {
		id := strconv.Itoa(i)
		existing = append(existing, config.Session{ID: id, Label: strings.Repeat("o", 200), Host: "h", Port: 22, User: "u"})
		dups = append(dups, config.Session{ID: id, Label: strings.Repeat("n", 200), Host: "h", Port: 22, User: "u"})
	}
	d = describeOverwrite(existing, dups)
	if n := strings.Count(d, "• "); n != sessMaxListed || !strings.Contains(d, "…and 5 more") {
		t.Fatalf("%d sessions listed: %q", n, d)
	}
	for _, line := range strings.Split(d, "\n") {
		if n := utf8.RuneCountInString(line); n > 2*sessMaxShown+20 {
			t.Fatalf("line of %d characters: %q", n, line)
		}
	}
}

func TestOverwritePasswordDetails(t *testing.T) {
	existing := []config.Session{
		{ID: "replaced", Label: "replaced", Host: "h", Port: 22, User: "u", Password: "current-pw"},
		{ID: "same", Label: "same", Host: "h", Port: 22, User: "u", Password: "current-pw"},
		{ID: "added", Label: "added", Host: "h", Port: 22, User: "u"},
		{ID: "kept", Label: "kept", Host: "h", Port: 22, User: "u", Password: "current-pw"},
	}
	dups := []config.Session{
		{ID: "replaced", Label: "replaced", Host: "h", Port: 22, User: "u", Password: "stale-pw"},
		{ID: "same", Label: "same", Host: "h", Port: 22, User: "u", Password: "current-pw"},
		{ID: "added", Label: "added", Host: "h", Port: 22, User: "u", Password: "new-pw"},
		{ID: "kept", Label: "kept", Host: "h", Port: 22, User: "u"},
	}
	// An imported password is used even for the same account...
	merged := overwriteSessions(append([]config.Session(nil), existing...), dups)
	if merged[0].Password != "stale-pw" || merged[3].Password != "current-pw" {
		t.Fatalf("passwords: %q, %q", merged[0].Password, merged[3].Password)
	}
	// ...so the description says so, without showing any password.
	d := describeOverwrite(existing, dups)
	if strings.Contains(d, "-pw") {
		t.Fatalf("password shown: %q", d)
	}
	blocks := strings.Split(d, "• ")[1:]
	if len(blocks) != len(dups) {
		t.Fatalf("description: %q", d)
	}
	for i, want := range []string{"stored password replaced", "", "password added", ""} {
		if want == "" {
			if strings.Contains(blocks[i], "password") {
				t.Errorf("%s: %q", dups[i].ID, blocks[i])
			}
		} else if !strings.Contains(blocks[i], want) {
			t.Errorf("%s: lacks %q: %q", dups[i].ID, want, blocks[i])
		}
	}
}

func TestParseImportLookalikes(t *testing.T) {
	existing := []config.Session{
		{ID: "prod", Label: "prod-db", Group: "Production", Host: "db.example", Port: 22, User: "admin"},
		{ID: "web", Label: "web", Host: "web.example", Port: 22, User: "u"},
	}
	long := strings.Repeat("u", 200)
	data := fmt.Sprintf(`[
		{"label": "prod-db", "group": "Production", "host": "evil.example", "user": "admin"},
		{"label": "PROD-DB", "group": "production", "host": "db.example", "port": 2222, "user": "admin"},
		{"label": "prod-db", "group": "Production", "host": "DB.example", "user": "admin"},
		{"label": "prod-db", "group": "Other", "host": "evil.example", "user": "admin"},
		{"label": "web", "host": "::1", "user": %q}
	]`, long)
	res, err := parseImport([]byte(data), existing)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.added) != 5 || len(res.lookalikes) != 3 {
		t.Fatalf("added %d, lookalikes %+v", len(res.added), res.lookalikes)
	}
	msg := res.summary(7)
	for _, want := range []string{
		"• 5 new sessions added",
		"same label and group as an existing session",
		"• prod-db (Production): admin@evil.example:22\n",
		"• PROD-DB (production): admin@db.example:2222\n",
		"• web: " + sessTruncate(long, sessMaxShown) + "@[::1]:22",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("summary lacks %q: %s", want, msg)
		}
	}
	// Same account, or another group: not listed.
	if strings.Contains(msg, "DB.example") || strings.Contains(msg, "(Other)") {
		t.Errorf("summary lists sessions that are no lookalikes: %s", msg)
	}

	// Many lookalikes: a bounded list.
	var b strings.Builder
	for i := 0; i < 25; i++ {
		fmt.Fprintf(&b, `{"label": "web", "host": "h%d.example", "user": "u"},`, i)
	}
	res, err = parseImport([]byte("["+strings.TrimSuffix(b.String(), ",")+"]"), existing)
	if err != nil {
		t.Fatal(err)
	}
	msg = res.summary(27)
	if n := strings.Count(msg, "• web: "); n != sessMaxListed || !strings.Contains(msg, "…and 5 more") {
		t.Fatalf("%d lookalikes listed: %s", n, msg)
	}
}

func TestExportFailedRemovesEmptyFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, data string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cause := errors.New("disk full")

	// The file the save dialog created (or truncated) is removed.
	empty := write("empty.json", "")
	err := sessExportFailed(empty, cause)
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "nothing was written") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Lstat(empty); !os.IsNotExist(err) {
		t.Fatal("empty export file left behind")
	}

	// Anything else stays: a file with data, a directory, a symlink.
	full := write("full.json", "data")
	sessExportFailed(full, cause)
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0700); err != nil {
		t.Fatal(err)
	}
	sessExportFailed(sub, cause)
	keep := []string{full, sub}
	if runtime.GOOS != "windows" {
		target := write("target", "")
		link := filepath.Join(dir, "link.json")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		sessExportFailed(link, cause)
		keep = append(keep, target, link)
	}
	for _, p := range keep {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s removed: %v", filepath.Base(p), err)
		}
	}
	sessExportFailed(filepath.Join(dir, "missing"), cause) // nothing to remove
}

func TestWritePrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "export.json")
	// The save dialog creates the file with default permissions first.
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := writePrivateFile(path, []byte("data"), true); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "data" {
		t.Fatalf("content %q", data)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0600 {
			t.Fatalf("mode %v, want 0600", fi.Mode().Perm())
		}
	}
}
