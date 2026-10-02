package logger

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

var (
	fileLogger *log.Logger
	logFile    *os.File
	mu         sync.Mutex
)

// retention is how long log files are kept; they contain host names and
// connection times.
const retention = 30 * 24 * time.Hour

// Init sets up file-based logging under ~/.mtssh/logs/ and deletes log
// files older than retention.
func Init() error {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || !filepath.IsAbs(home) {
		// No fallback to the current directory: it may belong to someone
		// else, who could put a symlink where the log file goes. A relative
		// home directory would be one, too; config.Dir refuses it the same way.
		return errors.New("cannot determine the home directory; logging to the console only")
	}
	dir := filepath.Join(home, ".mtssh", "logs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	removeOldLogs(dir, time.Now().Add(-retention))

	name := fmt.Sprintf("mtssh_%s.log", time.Now().Format("2006-01-02"))
	path := filepath.Join(dir, name)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	logFile = f
	fileLogger = log.New(f, "", log.LstdFlags)
	return nil
}

// removeOldLogs deletes MTSSH log files in dir last modified before before.
// Names are matched on their own, so glob characters in dir (e.g. a home
// directory containing "[") cannot select files elsewhere.
func removeOldLogs(dir string, before time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if ok, _ := filepath.Match("mtssh_*.log", e.Name()); !ok || !e.Type().IsRegular() {
			continue
		}
		if fi, err := e.Info(); err == nil && fi.ModTime().Before(before) {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// Clean makes text safe to log or show in the terminal: control and
// invisible format characters (newlines, escape sequences, bidi overrides)
// and the Unicode line and paragraph separators are replaced by Go escapes
// such as \n, \x1b or \u2028. Session labels, host names and error messages
// can come from import files or from the server.
func Clean(s string) string {
	if !strings.ContainsFunc(s, unsafeRune) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if unsafeRune(r) {
			q := strconv.QuoteRuneToASCII(r)
			b.WriteString(q[1 : len(q)-1])
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// unsafeRune reports the characters Clean escapes. U+2028 and U+2029
// (categories Zl, Zp) are not control characters, but many log viewers and
// editors start a new line at them, so they could forge log entries.
func unsafeRune(r rune) bool {
	return unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) || r == '\uFFFD'
}

// Info logs an informational message
func Info(session, msg string) {
	entry := fmt.Sprintf("[%s] INFO  %s", Clean(session), Clean(msg))
	fmt.Println(entry)
	mu.Lock()
	if fileLogger != nil {
		fileLogger.Println(entry)
	}
	mu.Unlock()
}

// Error logs an error message
func Error(session, msg string) {
	entry := fmt.Sprintf("[%s] ERROR %s", Clean(session), Clean(msg))
	fmt.Fprintln(os.Stderr, entry)
	mu.Lock()
	if fileLogger != nil {
		fileLogger.Println(entry)
	}
	mu.Unlock()
}

// Close flushes and closes the log file
func Close() {
	mu.Lock()
	defer mu.Unlock()
	if logFile != nil {
		logFile.Close()
		logFile = nil
		fileLogger = nil
	}
}
