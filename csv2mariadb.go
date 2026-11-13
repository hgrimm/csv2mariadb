// Copyright 2026 Herwig Grimm
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// csv2mariadb imports a German CSV file into a MariaDB table.
//
// The input is semicolon-separated with double-quoted fields and CRLF or LF
// line endings, encoded as UTF-8 with BOM, UTF-16 LE/BE with BOM or
// Windows-1252. The first line provides the column names: umlauts become
// ae/oe/ue, ß becomes ss and whitespace becomes an underscore.
//
// Only the Go standard library is used. Instead of an external database
// driver, the program contains a small client for the MariaDB/MySQL wire
// protocol (handshake, mysql_native_password, optional TLS, text queries).
//
// Build:   go build -o csv2mariadb csv2mariadb.go
// Usage:   csv2mariadb [flags] file.csv      (see csv2mariadb -h)
package main

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

const configFileName = "csv2mariadb.conf"

// ============================================================================
// Command line
// ============================================================================

type options struct {
	file        string
	table       string
	configPath  string
	ifExists    string
	pk          string
	usePK       string
	encoding    string
	sep         rune
	infer       bool
	lazyQuotes  bool
	lenient     bool
	ascii       bool
	emptyString bool
	dryRun      bool
	batch       int
}

const usageHeader = `csv2mariadb – import a German CSV file into a MariaDB table

Usage:
  csv2mariadb [flags] file.csv

The first CSV line defines the column names. The database connection is read
from a configuration file (key = value), searched in this order:
  -config <path>, ./csv2mariadb.conf, ~/.csv2mariadb.conf,
  csv2mariadb.conf next to the executable.
The environment variable CSV2MARIADB_PASSWORD overrides the password.

Flags:
`

func parseFlags(args []string) (*options, error) {
	o := &options{}
	var sep string
	fs := flag.NewFlagSet("csv2mariadb", flag.ContinueOnError)
	fs.StringVar(&o.table, "table", "", "target table `name` (default: derived from the file name)")
	fs.StringVar(&o.configPath, "config", "", "configuration `file` with the MariaDB access data")
	fs.StringVar(&o.ifExists, "if-exists", "fail", "if the table exists: fail | append | truncate | replace")
	fs.BoolVar(&o.infer, "infer", false, "detect column types (BIGINT, DECIMAL, DATE, DATETIME) from German formats\ninstead of storing everything as TEXT")
	fs.StringVar(&o.pk, "pk", "", "add an AUTO_INCREMENT primary key `column` with this name")
	fs.StringVar(&o.usePK, "use-pk", "", "use existing CSV `column`(s) as primary key (comma-separated for a composite key);\nno AUTO_INCREMENT column is added. Applies when the table is created")
	fs.StringVar(&o.encoding, "encoding", "auto", "input encoding: auto | utf8 | utf16le | utf16be | cp1252")
	fs.StringVar(&sep, "sep", ";", "field separator (a single character, or \"tab\")")
	fs.BoolVar(&o.lazyQuotes, "lazy-quotes", false, "tolerate stray quotes inside unquoted or quoted fields")
	fs.BoolVar(&o.lenient, "lenient", false, "fill missing trailing fields of short rows with NULL instead of failing")
	fs.BoolVar(&o.ascii, "ascii", false, "additionally replace every character outside [A-Za-z0-9_] in names with '_'")
	fs.BoolVar(&o.emptyString, "empty-string", false, "store empty text fields as '' instead of NULL")
	fs.IntVar(&o.batch, "batch", 1000, "maximum `rows` per INSERT statement")
	fs.BoolVar(&o.dryRun, "dry-run", false, "only analyse the file and print the CREATE TABLE statement")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), usageHeader)
		fs.PrintDefaults()
	}

	// Allow flags before and after the file name.
	var files []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			break
		}
		files = append(files, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(files) != 1 {
		fs.Usage()
		return nil, errors.New("exactly one CSV file must be given")
	}
	o.file = files[0]

	switch o.ifExists {
	case "fail", "append", "truncate", "replace":
	default:
		return nil, fmt.Errorf("invalid -if-exists %q (fail, append, truncate or replace)", o.ifExists)
	}
	if o.batch < 1 {
		return nil, errors.New("-batch must be at least 1")
	}
	switch strings.ToLower(sep) {
	case "tab", `\t`:
		sep = "\t"
	}
	if utf8.RuneCountInString(sep) != 1 {
		return nil, fmt.Errorf("-sep must be exactly one character, got %q", sep)
	}
	o.sep, _ = utf8.DecodeRuneInString(sep)
	if !validSeparator(o.sep) {
		return nil, fmt.Errorf("invalid separator %q", sep)
	}
	enc, err := normalizeEncoding(o.encoding)
	if err != nil {
		return nil, err
	}
	o.encoding = enc

	if o.table == "" {
		base := filepath.Base(o.file)
		o.table = strings.TrimSuffix(base, filepath.Ext(base))
	}
	o.table = sanitizeName(o.table, o.ascii)
	if o.table == "" {
		return nil, errors.New("table name is empty after normalisation")
	}
	if o.pk != "" && o.usePK != "" {
		return nil, errors.New("-pk and -use-pk cannot be combined: -use-pk defines the primary key from CSV columns")
	}
	if o.pk != "" {
		if o.pk = sanitizeName(o.pk, o.ascii); o.pk == "" {
			return nil, errors.New("-pk name is empty after normalisation")
		}
	}
	return o, nil
}

func validSeparator(r rune) bool {
	return r != '"' && r != '\r' && r != '\n' && r != utf8.RuneError && r != 0xFEFF
}

// ============================================================================
// Configuration file
// ============================================================================

type config struct {
	path      string
	host      string
	port      int
	socket    string
	user      string
	password  string
	database  string
	tlsMode   string // "false", "true" (verify) or "skip-verify"
	tlsCA     string
	collation string
	timeout   time.Duration
}

func loadConfig(explicit string) (*config, error) {
	var candidates []string
	if explicit != "" {
		candidates = []string{explicit}
	} else {
		candidates = append(candidates, configFileName)
		if home, err := os.UserHomeDir(); err == nil {
			candidates = append(candidates, filepath.Join(home, "."+configFileName))
		}
		if exe, err := os.Executable(); err == nil {
			candidates = append(candidates, filepath.Join(filepath.Dir(exe), configFileName))
		}
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil || explicit != "" {
			return parseConfig(p)
		}
	}
	return nil, fmt.Errorf("no configuration file found (looked for %s)", strings.Join(candidates, ", "))
}

func parseConfig(path string) (*config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	cfg := &config{
		path:      path,
		host:      "127.0.0.1",
		port:      3306,
		tlsMode:   "false",
		collation: "utf8mb4_unicode_ci",
		timeout:   10 * time.Second,
	}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(strings.TrimPrefix(sc.Text(), "\uFEFF"))
		if line == "" || line[0] == '#' || line[0] == ';' || line[0] == '[' {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected 'key = value'", path, n)
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' && val[len(val)-1] == '"' || val[0] == '\'' && val[len(val)-1] == '\'') {
			val = val[1 : len(val)-1]
		}
		switch key {
		case "host":
			cfg.host = val
		case "port":
			if cfg.port, err = strconv.Atoi(val); err != nil || cfg.port < 1 || cfg.port > 65535 {
				return nil, fmt.Errorf("%s:%d: invalid port %q", path, n, val)
			}
		case "socket":
			cfg.socket = val
		case "user":
			cfg.user = val
		case "password":
			cfg.password = val
		case "database":
			cfg.database = val
		case "tls":
			switch strings.ToLower(val) {
			case "", "false", "off", "no", "0":
				cfg.tlsMode = "false"
			case "true", "on", "yes", "1", "verify":
				cfg.tlsMode = "true"
			case "skip-verify":
				cfg.tlsMode = "skip-verify"
			default:
				return nil, fmt.Errorf("%s:%d: tls must be false, true or skip-verify", path, n)
			}
		case "tls_ca":
			cfg.tlsCA = val
		case "collation":
			cfg.collation = val
		case "timeout":
			if d, err := time.ParseDuration(val); err == nil {
				cfg.timeout = d
			} else if s, err := strconv.Atoi(val); err == nil {
				cfg.timeout = time.Duration(s) * time.Second
			} else {
				return nil, fmt.Errorf("%s:%d: invalid timeout %q", path, n, val)
			}
		default:
			return nil, fmt.Errorf("%s:%d: unknown key %q", path, n, key)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if pw, ok := os.LookupEnv("CSV2MARIADB_PASSWORD"); ok {
		cfg.password = pw
	}
	if cfg.user == "" {
		return nil, fmt.Errorf("%s: 'user' is missing", path)
	}
	if cfg.database == "" {
		return nil, fmt.Errorf("%s: 'database' is missing", path)
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_]+$`).MatchString(cfg.collation) || !strings.HasPrefix(cfg.collation, "utf8mb4_") {
		return nil, fmt.Errorf("%s: collation must be a utf8mb4_* collation, got %q", path, cfg.collation)
	}
	if runtime.GOOS != "windows" && cfg.password != "" {
		if fi, err := f.Stat(); err == nil && fi.Mode().Perm()&0o077 != 0 {
			fmt.Fprintf(os.Stderr, "warning: %s contains a password and is readable by others (chmod 600 recommended)\n", path)
		}
	}
	return cfg, nil
}

// ============================================================================
// Identifier normalisation
// ============================================================================

var umlautReplacer = strings.NewReplacer(
	"ä", "ae", "ö", "oe", "ü", "ue", "Ä", "Ae", "Ö", "Oe", "Ü", "Ue", "ß", "ss", "ẞ", "SS",
	// decomposed forms (NFD), e.g. from files created on macOS
	"a\u0308", "ae", "o\u0308", "oe", "u\u0308", "ue", "A\u0308", "Ae", "O\u0308", "Oe", "U\u0308", "Ue",
)

const maxIdentLen = 64 // MariaDB limit for table and column names (characters)

// sanitizeName applies the naming rules: umlauts -> ae/oe/ue, ß -> ss,
// whitespace -> '_'. With ascii=true every other character outside
// [A-Za-z0-9_] is replaced by '_' as well.
func sanitizeName(s string, ascii bool) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\uFEFF", ""))
	s = umlautReplacer.Replace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == 0 || unicode.IsControl(r) && !unicode.IsSpace(r):
			// drop
		case unicode.IsSpace(r):
			b.WriteByte('_')
		case ascii && !(r < utf8.RuneSelf && (r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r))):
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	name := b.String()
	if ascii {
		for strings.Contains(name, "__") {
			name = strings.ReplaceAll(name, "__", "_")
		}
		name = strings.Trim(name, "_")
	}
	return truncateRunes(name, maxIdentLen)
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

// columnNames derives unique column names from the header. MariaDB column
// names are case-insensitive, so duplicates are detected case-insensitively.
func columnNames(header []string, ascii bool, reserved string) []string {
	names := make([]string, len(header))
	seen := map[string]bool{}
	if reserved != "" {
		seen[strings.ToLower(reserved)] = true
	}
	for i, h := range header {
		name := sanitizeName(h, ascii)
		if name == "" {
			name = fmt.Sprintf("spalte_%d", i+1)
		}
		base := name
		for k := 2; seen[strings.ToLower(name)]; k++ {
			suffix := "_" + strconv.Itoa(k)
			name = truncateRunes(base, maxIdentLen-len(suffix)) + suffix
		}
		seen[strings.ToLower(name)] = true
		names[i] = name
	}
	return names
}

func quoteIdent(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

// appendSQLString appends s as a single-quoted SQL string literal. The
// session runs without NO_BACKSLASH_ESCAPES (see prepareSession), so the
// backslash escapes are valid. UTF-8 multi-byte sequences never contain the
// escaped bytes, so byte-wise processing is safe.
func appendSQLString(buf []byte, s string) []byte {
	buf = append(buf, '\'')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case 0:
			buf = append(buf, '\\', '0')
		case '\n':
			buf = append(buf, '\\', 'n')
		case '\r':
			buf = append(buf, '\\', 'r')
		case '\\':
			buf = append(buf, '\\', '\\')
		case '\'':
			buf = append(buf, '\\', '\'')
		case '"':
			buf = append(buf, '\\', '"')
		case 0x1a:
			buf = append(buf, '\\', 'Z')
		default:
			buf = append(buf, c)
		}
	}
	return append(buf, '\'')
}

func sqlString(s string) string { return string(appendSQLString(nil, s)) }

// ============================================================================
// Input decoding (BOM detection, UTF-16, Windows-1252) and CSV reading
// ============================================================================

func normalizeEncoding(e string) (string, error) {
	switch strings.ToLower(strings.ReplaceAll(e, "-", "")) {
	case "auto", "":
		return "auto", nil
	case "utf8":
		return "utf8", nil
	case "utf16", "utf16le", "unicode":
		return "utf16le", nil
	case "utf16be":
		return "utf16be", nil
	case "cp1252", "windows1252", "ansi", "latin1", "iso88591":
		return "cp1252", nil
	}
	return "", fmt.Errorf("unknown encoding %q (auto, utf8, utf16le, utf16be, cp1252)", e)
}

var (
	bomUTF8    = []byte{0xEF, 0xBB, 0xBF}
	bomUTF16LE = []byte{0xFF, 0xFE}
	bomUTF16BE = []byte{0xFE, 0xFF}
)

var warnedCP1252 bool // the file is opened up to three times; warn once

func detectEncoding(br *bufio.Reader, want string) string {
	head, _ := br.Peek(4096)
	if want != "auto" {
		return want
	}
	switch {
	case bytes.HasPrefix(head, bomUTF8):
		return "utf8"
	case bytes.HasPrefix(head, bomUTF16LE):
		return "utf16le"
	case bytes.HasPrefix(head, bomUTF16BE):
		return "utf16be"
	}
	// No BOM: UTF-16 text has many NUL bytes in every second position.
	var even, odd int
	for i, b := range head {
		if b == 0 {
			if i%2 == 0 {
				even++
			} else {
				odd++
			}
		}
	}
	switch {
	case odd > len(head)/4:
		return "utf16le"
	case even > len(head)/4:
		return "utf16be"
	}
	valid := utf8.Valid(head)
	for k := 1; !valid && k <= 3 && len(head) == 4096; k++ { // rune cut at buffer end
		valid = utf8.Valid(head[:len(head)-k])
	}
	if !valid {
		if !warnedCP1252 {
			warnedCP1252 = true
			fmt.Fprintln(os.Stderr, "warning: no BOM and not valid UTF-8 – assuming Windows-1252 (override with -encoding)")
		}
		return "cp1252"
	}
	return "utf8"
}

// utf16Reader converts a UTF-16 byte stream into UTF-8.
type utf16Reader struct {
	src *bufio.Reader
	be  bool
	out []byte
	err error
}

func (u *utf16Reader) unit() (uint16, error) {
	var b [2]byte
	if _, err := io.ReadFull(u.src, b[:]); err != nil {
		if err == io.ErrUnexpectedEOF {
			err = errors.New("UTF-16 input has an odd number of bytes")
		}
		return 0, err
	}
	if u.be {
		return binary.BigEndian.Uint16(b[:]), nil
	}
	return binary.LittleEndian.Uint16(b[:]), nil
}

func (u *utf16Reader) Read(p []byte) (int, error) {
	for len(u.out) == 0 {
		if u.err != nil {
			return 0, u.err
		}
		buf := u.out[:0]
		for i := 0; i < 4096 && u.err == nil; i++ {
			c, err := u.unit()
			if err != nil {
				u.err = err
				break
			}
			r := rune(c)
			if utf16.IsSurrogate(r) {
				c2, err := u.unit()
				if err == nil {
					r = utf16.DecodeRune(r, rune(c2))
				}
				if err != nil || r == utf8.RuneError {
					u.err = errors.New("invalid UTF-16 surrogate pair in input")
					break
				}
			}
			buf = utf8.AppendRune(buf, r)
		}
		u.out = buf
	}
	n := copy(p, u.out)
	u.out = u.out[n:]
	return n, nil
}

// cp1252Reader converts Windows-1252 (superset of ISO-8859-1) into UTF-8.
type cp1252Reader struct {
	src io.Reader
	in  []byte
	out []byte
}

var cp1252High = [32]rune{
	0x20AC, 0x0081, 0x201A, 0x0192, 0x201E, 0x2026, 0x2020, 0x2021,
	0x02C6, 0x2030, 0x0160, 0x2039, 0x0152, 0x008D, 0x017D, 0x008F,
	0x0090, 0x2018, 0x2019, 0x201C, 0x201D, 0x2022, 0x2013, 0x2014,
	0x02DC, 0x2122, 0x0161, 0x203A, 0x0153, 0x009D, 0x017E, 0x0178,
}

func (c *cp1252Reader) Read(p []byte) (int, error) {
	if len(c.out) == 0 {
		if c.in == nil {
			c.in = make([]byte, 16*1024)
		}
		n, err := c.src.Read(c.in)
		buf := c.out[:0]
		for _, b := range c.in[:n] {
			switch {
			case b < 0x80:
				buf = append(buf, b)
			case b < 0xA0:
				buf = utf8.AppendRune(buf, cp1252High[b-0x80])
			default:
				buf = utf8.AppendRune(buf, rune(b))
			}
		}
		c.out = buf
		if len(c.out) == 0 {
			return 0, err
		}
	}
	n := copy(p, c.out)
	c.out = c.out[n:]
	return n, nil
}

// source is one pass over the CSV file.
type source struct {
	f          *os.File
	r          *csv.Reader
	encoding   string
	sep        rune
	lineOffset int // lines consumed before the CSV reader (e.g. "sep=;")
	checkUTF8  bool
}

func openSource(o *options) (*source, error) {
	f, err := os.Open(o.file)
	if err != nil {
		return nil, err
	}
	br := bufio.NewReaderSize(f, 64*1024)
	enc := detectEncoding(br, o.encoding)
	head, _ := br.Peek(3)

	var dec io.Reader
	switch enc {
	case "utf8":
		if bytes.HasPrefix(head, bomUTF8) {
			br.Discard(3)
		}
		dec = br
	case "utf16le", "utf16be":
		if bytes.HasPrefix(head, bomUTF16LE) || bytes.HasPrefix(head, bomUTF16BE) {
			br.Discard(2)
		}
		dec = &utf16Reader{src: br, be: enc == "utf16be"}
	case "cp1252":
		dec = &cp1252Reader{src: br}
	}

	s := &source{f: f, encoding: enc, sep: o.sep, checkUTF8: enc == "utf8"}
	tr := bufio.NewReaderSize(dec, 64*1024)

	// Excel separator hint in the first line, e.g. "sep=;".
	if p, _ := tr.Peek(4); strings.EqualFold(string(p), "sep=") {
		line, _ := tr.ReadString('\n')
		v := strings.TrimRight(line[4:], "\r\n")
		r, size := utf8.DecodeRuneInString(v)
		if size != len(v) || !validSeparator(r) {
			f.Close()
			return nil, fmt.Errorf("line 1: invalid separator declaration %q", strings.TrimSpace(line))
		}
		s.sep = r
		s.lineOffset = 1
	}

	s.r = csv.NewReader(tr)
	s.r.Comma = s.sep
	s.r.FieldsPerRecord = -1 // field counts are checked by shapeRecord
	s.r.LazyQuotes = o.lazyQuotes
	s.r.ReuseRecord = true
	return s, nil
}

func (s *source) Close() error { return s.f.Close() }

// next returns the next record and its (1-based) line number.
func (s *source) next() ([]string, int, error) {
	rec, err := s.r.Read()
	if err != nil {
		var pe *csv.ParseError
		if errors.As(err, &pe) {
			pe.StartLine += s.lineOffset
			pe.Line += s.lineOffset
		}
		return nil, 0, err
	}
	line, _ := s.r.FieldPos(0)
	line += s.lineOffset
	if s.checkUTF8 {
		for i, f := range rec {
			if !utf8.ValidString(f) {
				return nil, line, fmt.Errorf("line %d, field %d: invalid UTF-8 (try -encoding cp1252)", line, i+1)
			}
		}
	}
	return rec, line, nil
}

// readHeader opens a source and returns it together with the header fields.
func readHeader(o *options) (*source, []string, error) {
	src, err := openSource(o)
	if err != nil {
		return nil, nil, err
	}
	rec, _, err := src.next()
	if err == io.EOF {
		src.Close()
		return nil, nil, errors.New("the CSV file is empty")
	}
	if err != nil {
		src.Close()
		return nil, nil, err
	}
	header := append([]string(nil), rec...)
	// Trailing separators in the header ("a;b;c;") produce empty names.
	for len(header) > 0 && strings.TrimSpace(header[len(header)-1]) == "" {
		header = header[:len(header)-1]
	}
	if len(header) == 0 {
		src.Close()
		return nil, nil, errors.New("the header line contains no column names")
	}
	return src, header, nil
}

// shapeRecord skips empty rows and checks the field count against the header.
func shapeRecord(rec []string, n int, lenient bool, line int) ([]string, bool, error) {
	empty := true
	for _, f := range rec {
		if strings.TrimSpace(f) != "" {
			empty = false
			break
		}
	}
	if empty {
		return nil, true, nil
	}
	if len(rec) > n {
		for _, f := range rec[n:] {
			if strings.TrimSpace(f) != "" {
				return nil, false, fmt.Errorf("line %d: %d fields, but the header has only %d columns", line, len(rec), n)
			}
		}
		rec = rec[:n]
	}
	if len(rec) < n {
		if !lenient {
			return nil, false, fmt.Errorf("line %d: %d fields, but the header has %d columns (use -lenient to fill with NULL)", line, len(rec), n)
		}
		for len(rec) < n {
			rec = append(rec, "")
		}
	}
	return rec, false, nil
}

// ============================================================================
// German value formats and type inference
// ============================================================================

type kind int

const (
	kindText kind = iota
	kindInt
	kindDecimal
	kindDate
	kindDateTime
)

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// parseGermanNumber accepts "1234", "-1.234.567", "1234,5", "1.234,56".
// Numbers with leading zeros ("0815", postal codes) are rejected so that
// they stay text. norm is the SQL form ("1234567.89").
func parseGermanNumber(s string) (norm string, intDigits, scale int, ok bool) {
	neg := false
	if strings.HasPrefix(s, "-") || strings.HasPrefix(s, "+") {
		neg = s[0] == '-'
		s = s[1:]
	}
	ip, fp, hasFrac := strings.Cut(s, ",")
	if hasFrac && !allDigits(fp) {
		return "", 0, 0, false
	}
	if strings.Contains(ip, ".") { // thousands separators: groups of three
		groups := strings.Split(ip, ".")
		if len(groups[0]) > 3 || !allDigits(groups[0]) || groups[0][0] == '0' {
			return "", 0, 0, false
		}
		for _, g := range groups[1:] {
			if len(g) != 3 || !allDigits(g) {
				return "", 0, 0, false
			}
		}
		ip = strings.Join(groups, "")
	} else if !allDigits(ip) || len(ip) > 1 && ip[0] == '0' {
		return "", 0, 0, false
	}
	norm = ip
	if hasFrac {
		norm += "." + fp
	}
	if neg {
		norm = "-" + norm
	}
	return norm, len(ip), len(fp), true
}

var (
	dateLayouts     = []string{"2.1.2006", "2006-01-02"}
	dateTimeLayouts = []string{"2.1.2006 15:04:05", "2.1.2006 15:04", "2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02 15:04"}
)

// parseGermanTime accepts "31.12.2025", "1.2.2025", "31.12.2025 14:30[:15]"
// and the ISO forms.
func parseGermanTime(s string) (t time.Time, dateOnly bool, ok bool) {
	if len(s) < 8 || s[0] < '0' || s[0] > '9' {
		return t, false, false
	}
	for _, l := range dateLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, true, true
		}
	}
	for _, l := range dateTimeLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, false, true
		}
	}
	return t, false, false
}

type colStats struct {
	nonEmpty, maxLen, intDigits, scale       int
	notInt, notDecimal, notDate, notTemporal bool
}

func (s *colStats) observe(raw string) {
	if n := utf8.RuneCountInString(raw); n > s.maxLen {
		s.maxLen = n
	}
	v := strings.TrimSpace(raw)
	if v == "" {
		return
	}
	s.nonEmpty++
	if !s.notDecimal {
		norm, id, sc, ok := parseGermanNumber(v)
		if !ok {
			s.notInt, s.notDecimal = true, true
		} else {
			s.intDigits = max(s.intDigits, id)
			s.scale = max(s.scale, sc)
			if sc > 0 {
				s.notInt = true
			} else if _, err := strconv.ParseInt(norm, 10, 64); err != nil {
				s.notInt = true
			}
		}
	}
	if !s.notTemporal {
		_, dateOnly, ok := parseGermanTime(v)
		if !ok {
			s.notDate, s.notTemporal = true, true
		} else if !dateOnly {
			s.notDate = true
		}
	}
}

func (s *colStats) decide() (kind, string) {
	switch {
	case s.nonEmpty == 0:
		return kindText, "TEXT"
	case !s.notInt:
		return kindInt, "BIGINT"
	case !s.notDecimal && s.intDigits+s.scale <= 65 && s.scale <= 38:
		return kindDecimal, fmt.Sprintf("DECIMAL(%d,%d)", max(1, s.intDigits+s.scale), s.scale)
	case !s.notDate:
		return kindDate, "DATE"
	case !s.notTemporal:
		return kindDateTime, "DATETIME"
	case s.maxLen <= 255:
		return kindText, "VARCHAR(255)"
	default:
		return kindText, "TEXT"
	}
}

// kindFromDBType maps an existing column type (information_schema DATA_TYPE)
// to the conversion applied to the CSV values.
func kindFromDBType(t string) kind {
	switch strings.ToLower(t) {
	case "tinyint", "smallint", "mediumint", "int", "integer", "bigint":
		return kindInt
	case "decimal", "numeric", "float", "double", "real":
		return kindDecimal
	case "date":
		return kindDate
	case "datetime", "timestamp":
		return kindDateTime
	}
	return kindText
}

// convertValue turns a CSV field into an SQL value. null=true means NULL.
func convertValue(k kind, raw string, emptyString bool) (val string, null bool, err error) {
	if k == kindText {
		if raw == "" && !emptyString {
			return "", true, nil
		}
		return raw, false, nil
	}
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", true, nil
	}
	switch k {
	case kindInt:
		norm, _, scale, ok := parseGermanNumber(v)
		if !ok || scale != 0 {
			return "", false, fmt.Errorf("%q is not an integer", raw)
		}
		return norm, false, nil
	case kindDecimal:
		norm, _, _, ok := parseGermanNumber(v)
		if !ok {
			return "", false, fmt.Errorf("%q is not a number in German format", raw)
		}
		return norm, false, nil
	case kindDate:
		t, dateOnly, ok := parseGermanTime(v)
		if !ok || !dateOnly && (t.Hour() != 0 || t.Minute() != 0 || t.Second() != 0) {
			return "", false, fmt.Errorf("%q is not a date (DD.MM.YYYY)", raw)
		}
		return t.Format("2006-01-02"), false, nil
	case kindDateTime:
		t, _, ok := parseGermanTime(v)
		if !ok {
			return "", false, fmt.Errorf("%q is not a date/time (DD.MM.YYYY HH:MM[:SS])", raw)
		}
		return t.Format("2006-01-02 15:04:05"), false, nil
	}
	return v, false, nil
}

// resolvePK maps the -use-pk specification onto column indexes. A part may
// be given as normalised column name or as original header text.
func resolvePK(spec string, header, names []string, ascii bool) ([]int, error) {
	var idx []int
	used := map[int]bool{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("-use-pk %q contains an empty column name", spec)
		}
		found := -1
		for _, match := range []func(i int) bool{
			func(i int) bool { return strings.EqualFold(names[i], part) },
			func(i int) bool { return strings.EqualFold(strings.TrimSpace(header[i]), part) },
			func(i int) bool { return strings.EqualFold(names[i], sanitizeName(part, ascii)) },
		} {
			for i := range names {
				if match(i) {
					found = i
					break
				}
			}
			if found >= 0 {
				break
			}
		}
		if found < 0 {
			return nil, fmt.Errorf("-use-pk: column %q not found; available: %s", part, strings.Join(names, ", "))
		}
		if used[found] {
			return nil, fmt.Errorf("-use-pk: column %q is listed twice", names[found])
		}
		used[found] = true
		idx = append(idx, found)
	}
	return idx, nil
}

// pkKeyType returns a column type usable in a primary key: InnoDB cannot
// index TEXT without a prefix length, so text columns become VARCHAR.
func pkKeyType(t string, maxLen int) (string, error) {
	if t != "TEXT" && t != "VARCHAR(255)" {
		return t, nil
	}
	const maxKeyChars = 768 // 3072 bytes InnoDB key limit / 4 bytes per utf8mb4 character
	if maxLen > maxKeyChars {
		return "", fmt.Errorf("values up to %d characters are too long for a primary key (max %d)", maxLen, maxKeyChars)
	}
	return fmt.Sprintf("VARCHAR(%d)", max(255, maxLen)), nil
}

// createTableSQL builds the CREATE TABLE statement. Either pk (an additional
// AUTO_INCREMENT column) or pkCols (existing columns, -use-pk) defines the
// primary key; with pkCols no AUTO_INCREMENT column is added.
func createTableSQL(table string, header, names, types []string, pk string, pkCols []int, collation string) string {
	inPK := map[int]bool{}
	var keyCols []string
	if pk != "" {
		keyCols = append(keyCols, quoteIdent(pk))
	}
	for _, i := range pkCols {
		inPK[i] = true
		keyCols = append(keyCols, quoteIdent(names[i]))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE %s (\n", quoteIdent(table))
	if pk != "" {
		fmt.Fprintf(&b, "  %s BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,\n", quoteIdent(pk))
	}
	for i, n := range names {
		null := "NULL"
		if inPK[i] {
			null = "NOT NULL"
		}
		fmt.Fprintf(&b, "  %s %s %s", quoteIdent(n), types[i], null)
		if orig := strings.TrimSpace(header[i]); orig != n && orig != "" {
			fmt.Fprintf(&b, " COMMENT %s", sqlString(truncateRunes(orig, 1024)))
		}
		if i < len(names)-1 || len(keyCols) > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	if len(keyCols) > 0 {
		fmt.Fprintf(&b, "  PRIMARY KEY (%s)\n", strings.Join(keyCols, ", "))
	}
	fmt.Fprintf(&b, ") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=%s", collation)
	return b.String()
}

// ============================================================================
// Main program
// ============================================================================

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "csv2mariadb:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	start := time.Now()
	o, err := parseFlags(args)
	if err != nil {
		return err
	}

	src, header, err := readHeader(o)
	if err != nil {
		return err
	}
	src.Close()
	names := columnNames(header, o.ascii, o.pk)
	ncols := len(names)
	var pkCols []int
	if o.usePK != "" {
		if pkCols, err = resolvePK(o.usePK, header, names, o.ascii); err != nil {
			return err
		}
	}

	// Pass 1 (with -infer, -use-pk or -dry-run): validate rows, infer types
	// and check the primary key values (present and unique).
	types := make([]string, ncols)
	kinds := make([]kind, ncols)
	for i := range types {
		types[i] = "TEXT"
	}
	rowCount, skipped := -1, 0
	analyse := func() error {
		if rowCount >= 0 {
			return nil
		}
		src, _, err := readHeader(o)
		if err != nil {
			return err
		}
		defer src.Close()
		stats := make([]colStats, ncols)
		seenKeys := map[string]int{}
		var key strings.Builder
		rowCount = 0
		for {
			rec, line, err := src.next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			rec, skip, err := shapeRecord(rec, ncols, o.lenient, line)
			if err != nil {
				return err
			}
			if skip {
				skipped++
				continue
			}
			rowCount++
			for i, v := range rec {
				stats[i].observe(v)
			}
			if len(pkCols) > 0 {
				key.Reset()
				for _, i := range pkCols {
					v := strings.TrimSpace(rec[i])
					if v == "" {
						return fmt.Errorf("line %d: primary key column %s is empty", line, names[i])
					}
					key.WriteString(v)
					key.WriteByte(0)
				}
				if prev, dup := seenKeys[key.String()]; dup {
					return fmt.Errorf("line %d: duplicate primary key value (same as line %d)", line, prev)
				}
				seenKeys[key.String()] = line
			}
		}
		if o.infer {
			for i := range stats {
				kinds[i], types[i] = stats[i].decide()
			}
		}
		for _, i := range pkCols {
			t, err := pkKeyType(types[i], stats[i].maxLen)
			if err != nil {
				return fmt.Errorf("-use-pk column %s: %v", names[i], err)
			}
			types[i] = t
		}
		return nil
	}

	if o.dryRun {
		if err := analyse(); err != nil {
			return err
		}
		collation := "utf8mb4_unicode_ci"
		if cfg, err := loadConfig(o.configPath); err == nil {
			collation = cfg.collation
		}
		fmt.Printf("File:      %s (encoding %s, separator %q)\n", o.file, src.encoding, src.sep)
		fmt.Printf("Table:     %s\nRows:      %d (%d empty rows skipped)\n\nColumns:\n", o.table, rowCount, skipped)
		isPK := map[int]bool{}
		for _, i := range pkCols {
			isPK[i] = true
		}
		for i, n := range names {
			mark := ""
			if isPK[i] {
				mark = "  (primary key)"
			}
			fmt.Printf("  %3d  %-30s -> %-30s %s%s\n", i+1, strings.TrimSpace(header[i]), n, types[i], mark)
		}
		fmt.Printf("\n%s;\n", createTableSQL(o.table, header, names, types, o.pk, pkCols, collation))
		return nil
	}

	cfg, err := loadConfig(o.configPath)
	if err != nil {
		return err
	}
	c, err := dial(cfg)
	if err != nil {
		return fmt.Errorf("connect to MariaDB: %w", err)
	}
	defer c.close()
	maxStmt, err := prepareSession(c, cfg)
	if err != nil {
		return err
	}

	exists, isView, err := tableExists(c, o.table)
	if err != nil {
		return err
	}
	target := quoteIdent(cfg.database) + "." + quoteIdent(o.table)
	var inserted int

	switch {
	case exists && isView:
		return fmt.Errorf("%s is a view, not a table", target)

	case exists && o.ifExists == "fail":
		return fmt.Errorf("table %s already exists (use -if-exists append, truncate or replace)", target)

	case exists && (o.ifExists == "append" || o.ifExists == "truncate"):
		if o.pk != "" || o.usePK != "" {
			fmt.Fprintln(os.Stderr, "note: -pk/-use-pk only apply when a table is created; the existing table keeps its structure")
		}
		dbNames, dbKinds, err := matchExistingColumns(c, o.table, names)
		if err != nil {
			return err
		}
		inserted, skipped, err = loadRows(c, o, o.table, dbNames, dbKinds, maxStmt, o.ifExists == "truncate")
		if err != nil {
			return err
		}

	default: // new table, or replace an existing one
		if o.infer || len(pkCols) > 0 {
			if err := analyse(); err != nil {
				return err
			}
		}
		load := o.table
		if exists { // replace: load into a staging table, then swap atomically
			load = truncateRunes(o.table, maxIdentLen-len("_csv2mdb_new")) + "_csv2mdb_new"
			if _, err := c.exec("DROP TABLE IF EXISTS " + quoteIdent(load)); err != nil {
				return err
			}
		}
		if _, err := c.exec(createTableSQL(load, header, names, types, o.pk, pkCols, cfg.collation)); err != nil {
			return fmt.Errorf("create table: %w", err)
		}
		inserted, skipped, err = loadRows(c, o, load, names, kinds, maxStmt, false)
		if err != nil {
			c.exec("DROP TABLE IF EXISTS " + quoteIdent(load))
			return err
		}
		if exists {
			old := truncateRunes(o.table, maxIdentLen-len("_csv2mdb_old")) + "_csv2mdb_old"
			stmts := []string{
				"DROP TABLE IF EXISTS " + quoteIdent(old),
				fmt.Sprintf("RENAME TABLE %s TO %s, %s TO %s", quoteIdent(o.table), quoteIdent(old), quoteIdent(load), quoteIdent(o.table)),
				"DROP TABLE " + quoteIdent(old),
			}
			for _, s := range stmts {
				if _, err := c.exec(s); err != nil {
					return fmt.Errorf("replace table: %w", err)
				}
			}
			fmt.Fprintf(os.Stderr, "Replaced table %s (%d columns)\n", target, ncols)
		} else {
			fmt.Fprintf(os.Stderr, "Created table %s (%d columns)\n", target, ncols)
		}
	}

	fmt.Fprintf(os.Stderr, "Imported %d rows into %s", inserted, target)
	if skipped > 0 {
		fmt.Fprintf(os.Stderr, " (%d empty rows skipped)", skipped)
	}
	fmt.Fprintf(os.Stderr, " in %s\n", time.Since(start).Round(time.Millisecond))
	return nil
}

// prepareSession sets the connection character set and an SQL mode that
// makes the string escaping valid and invalid data fail loudly. It returns
// the maximum size of one INSERT statement.
func prepareSession(c *conn, cfg *config) (int, error) {
	if _, err := c.exec("SET NAMES utf8mb4 COLLATE " + cfg.collation); err != nil {
		return 0, err
	}
	_, rows, err := c.query("SELECT @@SESSION.sql_mode, @@max_allowed_packet")
	if err != nil {
		return 0, err
	}
	if len(rows) != 1 || rows[0][1] == nil {
		return 0, errors.New("unexpected answer to SELECT @@sql_mode")
	}
	var modes []string
	if rows[0][0] != nil {
		for _, m := range strings.Split(*rows[0][0], ",") {
			if m != "" && m != "NO_BACKSLASH_ESCAPES" && m != "STRICT_TRANS_TABLES" {
				modes = append(modes, m)
			}
		}
	}
	modes = append(modes, "STRICT_TRANS_TABLES")
	if _, err := c.exec("SET SESSION sql_mode = " + sqlString(strings.Join(modes, ","))); err != nil {
		return 0, err
	}
	maxPacket, _ := strconv.Atoi(*rows[0][1])
	maxStmt := 4 << 20
	if maxPacket > 0 && maxPacket-4096 < maxStmt {
		maxStmt = max(maxPacket-4096, 1024)
	}
	return maxStmt, nil
}

func tableExists(c *conn, table string) (exists, isView bool, err error) {
	_, rows, err := c.query("SELECT TABLE_TYPE FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = " + sqlString(table))
	if err != nil {
		return false, false, err
	}
	if len(rows) == 0 {
		return false, false, nil
	}
	return true, rows[0][0] != nil && *rows[0][0] == "VIEW", nil
}

// matchExistingColumns maps the CSV column names onto the columns of an
// existing table (case-insensitively) and returns their types.
func matchExistingColumns(c *conn, table string, names []string) ([]string, []kind, error) {
	_, rows, err := c.query("SELECT COLUMN_NAME, DATA_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = " +
		sqlString(table) + " ORDER BY ORDINAL_POSITION")
	if err != nil {
		return nil, nil, err
	}
	existing := map[string][2]string{}
	for _, r := range rows {
		if r[0] != nil && r[1] != nil {
			existing[strings.ToLower(*r[0])] = [2]string{*r[0], *r[1]}
		}
	}
	dbNames := make([]string, len(names))
	kinds := make([]kind, len(names))
	var missing []string
	for i, n := range names {
		col, ok := existing[strings.ToLower(n)]
		if !ok {
			missing = append(missing, n)
			continue
		}
		dbNames[i], kinds[i] = col[0], kindFromDBType(col[1])
	}
	if len(missing) > 0 {
		return nil, nil, fmt.Errorf("table %s has no column(s) %s", quoteIdent(table), strings.Join(missing, ", "))
	}
	return dbNames, kinds, nil
}

// loadRows reads the CSV (pass 2) and inserts all rows in one transaction
// using multi-row INSERT statements.
func loadRows(c *conn, o *options, table string, cols []string, kinds []kind, maxStmt int, deleteFirst bool) (n, skipped int, err error) {
	src, header, err := readHeader(o)
	if err != nil {
		return 0, 0, err
	}
	defer src.Close()

	if _, err = c.exec("START TRANSACTION"); err != nil {
		return 0, 0, err
	}
	defer func() {
		if err != nil {
			c.exec("ROLLBACK")
		}
	}()
	if deleteFirst {
		if _, err = c.exec("DELETE FROM " + quoteIdent(table)); err != nil {
			return 0, 0, err
		}
	}

	prefix := []byte("INSERT INTO " + quoteIdent(table) + " (")
	for i, col := range cols {
		if i > 0 {
			prefix = append(prefix, ',')
		}
		prefix = append(prefix, quoteIdent(col)...)
	}
	prefix = append(prefix, ") VALUES "...)

	stmt := append(make([]byte, 0, maxStmt+8192), prefix...)
	var row []byte
	inStmt, firstLine, lastLine, warned := 0, 0, 0, false

	flush := func() error {
		if inStmt == 0 {
			return nil
		}
		res, err := c.exec(stmt)
		if err != nil {
			return fmt.Errorf("insert of CSV lines %d–%d failed: %w", firstLine, lastLine, err)
		}
		if res.warnings > 0 && !warned {
			warned = true
			printWarnings(c, firstLine, lastLine)
		}
		stmt = append(stmt[:0], prefix...)
		inStmt = 0
		return nil
	}

	for {
		rec, line, rerr := src.next()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return n, skipped, rerr
		}
		rec, skip, serr := shapeRecord(rec, len(cols), o.lenient, line)
		if serr != nil {
			return n, skipped, serr
		}
		if skip {
			skipped++
			continue
		}
		row = append(row[:0], '(')
		for i, raw := range rec {
			if i > 0 {
				row = append(row, ',')
			}
			v, null, cerr := convertValue(kinds[i], raw, o.emptyString)
			if cerr != nil {
				return n, skipped, fmt.Errorf("line %d, column %s: %v", line, strings.TrimSpace(header[i]), cerr)
			}
			if null {
				row = append(row, "NULL"...)
			} else {
				row = appendSQLString(row, v)
			}
		}
		row = append(row, ')')

		if inStmt > 0 && (inStmt >= o.batch || len(stmt)+1+len(row) > maxStmt) {
			if err = flush(); err != nil {
				return n, skipped, err
			}
		}
		if inStmt == 0 {
			firstLine = line
		} else {
			stmt = append(stmt, ',')
		}
		stmt = append(stmt, row...)
		inStmt++
		lastLine = line
		n++
		if n%100000 == 0 {
			fmt.Fprintf(os.Stderr, "... %d rows\n", n)
		}
	}
	if err = flush(); err != nil {
		return n, skipped, err
	}
	_, err = c.exec("COMMIT")
	return n, skipped, err
}

func printWarnings(c *conn, first, last int) {
	_, rows, err := c.query("SHOW WARNINGS LIMIT 5")
	if err != nil {
		return
	}
	fmt.Fprintf(os.Stderr, "warning: MariaDB reported warnings for CSV lines %d–%d:\n", first, last)
	for _, r := range rows {
		if len(r) == 3 && r[2] != nil {
			fmt.Fprintf(os.Stderr, "  %s\n", *r[2])
		}
	}
}

// ============================================================================
// Minimal MariaDB/MySQL client protocol (text protocol only)
// ============================================================================

const (
	clientLongPassword         = 1 << 0
	clientLongFlag             = 1 << 2
	clientConnectWithDB        = 1 << 3
	clientProtocol41           = 1 << 9
	clientSSL                  = 1 << 11
	clientTransactions         = 1 << 13
	clientSecureConnection     = 1 << 15
	clientMultiResults         = 1 << 17
	clientPluginAuth           = 1 << 19
	clientPluginAuthLenencData = 1 << 21

	maxPayload       = 1<<24 - 1
	charsetUTF8MB4   = 45 // utf8mb4_general_ci; the collation is set via SET NAMES
	comQuit          = 0x01
	comQuery         = 0x03
	serverMoreResult = 0x0008
)

var errMalformed = errors.New("malformed packet from server")

type mysqlError struct {
	code    uint16
	state   string
	message string
}

func (e *mysqlError) Error() string {
	if e.state != "" {
		return fmt.Sprintf("error %d (%s): %s", e.code, e.state, e.message)
	}
	return fmt.Sprintf("error %d: %s", e.code, e.message)
}

func parseErrPacket(p []byte) error {
	e := &mysqlError{}
	if len(p) >= 3 {
		e.code = binary.LittleEndian.Uint16(p[1:3])
		p = p[3:]
	}
	if len(p) >= 6 && p[0] == '#' {
		e.state = string(p[1:6])
		p = p[6:]
	}
	e.message = string(p)
	return e
}

type okResult struct {
	affected, insertID uint64
	status, warnings   uint16
}

type conn struct {
	nc   net.Conn
	br   *bufio.Reader
	seq  uint8
	caps uint32
}

func (c *conn) readPacket() ([]byte, error) {
	var payload []byte
	for {
		var hdr [4]byte
		if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
			return nil, err
		}
		n := int(hdr[0]) | int(hdr[1])<<8 | int(hdr[2])<<16
		if hdr[3] != c.seq {
			return nil, fmt.Errorf("protocol error: packet sequence %d, expected %d", hdr[3], c.seq)
		}
		c.seq++
		off := len(payload)
		payload = append(payload, make([]byte, n)...)
		if _, err := io.ReadFull(c.br, payload[off:]); err != nil {
			return nil, err
		}
		if n < maxPayload {
			if len(payload) == 0 {
				return nil, errMalformed
			}
			return payload, nil
		}
	}
}

func (c *conn) writePacket(data []byte) error {
	for {
		n := min(len(data), maxPayload)
		hdr := []byte{byte(n), byte(n >> 8), byte(n >> 16), c.seq}
		c.seq++
		bufs := net.Buffers{hdr, data[:n]}
		if _, err := bufs.WriteTo(c.nc); err != nil {
			return err
		}
		data = data[n:]
		if n < maxPayload {
			return nil
		}
	}
}

func readLenEnc(p []byte) (v uint64, size int, null bool, err error) {
	if len(p) == 0 {
		return 0, 0, false, errMalformed
	}
	switch b := p[0]; {
	case b < 0xfb:
		return uint64(b), 1, false, nil
	case b == 0xfb:
		return 0, 1, true, nil
	case b == 0xfc && len(p) >= 3:
		return uint64(binary.LittleEndian.Uint16(p[1:])), 3, false, nil
	case b == 0xfd && len(p) >= 4:
		return uint64(p[1]) | uint64(p[2])<<8 | uint64(p[3])<<16, 4, false, nil
	case b == 0xfe && len(p) >= 9:
		return binary.LittleEndian.Uint64(p[1:]), 9, false, nil
	}
	return 0, 0, false, errMalformed
}

func readLenEncString(p []byte) (s []byte, rest []byte, null bool, err error) {
	v, n, null, err := readLenEnc(p)
	if err != nil || null {
		return nil, p[n:], null, err
	}
	if uint64(len(p)-n) < v {
		return nil, nil, false, errMalformed
	}
	return p[n : n+int(v)], p[n+int(v):], false, nil
}

func appendLenEnc(b []byte, v uint64) []byte {
	switch {
	case v < 0xfb:
		return append(b, byte(v))
	case v <= 0xffff:
		return append(b, 0xfc, byte(v), byte(v>>8))
	case v <= 0xffffff:
		return append(b, 0xfd, byte(v), byte(v>>8), byte(v>>16))
	}
	b = append(b, 0xfe)
	return binary.LittleEndian.AppendUint64(b, v)
}

func isEOFPacket(p []byte) bool { return len(p) > 0 && p[0] == 0xfe && len(p) < 9 }

func parseOK(p []byte) (okResult, error) {
	var r okResult
	p = p[1:]
	v, n, _, err := readLenEnc(p)
	if err != nil {
		return r, err
	}
	r.affected, p = v, p[n:]
	if v, n, _, err = readLenEnc(p); err != nil {
		return r, err
	}
	r.insertID, p = v, p[n:]
	if len(p) >= 4 {
		r.status = binary.LittleEndian.Uint16(p)
		r.warnings = binary.LittleEndian.Uint16(p[2:])
	}
	return r, nil
}

func dial(cfg *config) (*conn, error) {
	network, addr := "tcp", net.JoinHostPort(cfg.host, strconv.Itoa(cfg.port))
	if cfg.socket != "" {
		network, addr = "unix", cfg.socket
	}
	nc, err := net.DialTimeout(network, addr, cfg.timeout)
	if err != nil {
		return nil, err
	}
	c := &conn{nc: nc, br: bufio.NewReaderSize(nc, 64*1024)}
	c.nc.SetDeadline(time.Now().Add(cfg.timeout))
	if err := c.handshake(cfg, network == "unix"); err != nil {
		c.nc.Close()
		return nil, err
	}
	c.nc.SetDeadline(time.Time{})
	return c, nil
}

func (c *conn) handshake(cfg *config, unixSocket bool) error {
	c.seq = 0
	pkt, err := c.readPacket()
	if err != nil {
		return err
	}
	if pkt[0] == 0xff {
		return parseErrPacket(pkt)
	}
	if pkt[0] != 10 {
		return fmt.Errorf("unsupported protocol version %d", pkt[0])
	}
	p := pkt[1:]
	i := bytes.IndexByte(p, 0)
	if i < 0 || len(p) < i+1+4+8+1+2 {
		return errMalformed
	}
	p = p[i+1+4:] // server version, connection id
	scramble := append([]byte(nil), p[:8]...)
	p = p[9:]
	serverCaps := uint32(binary.LittleEndian.Uint16(p))
	p = p[2:]
	plugin := "mysql_native_password"
	if len(p) >= 16 {
		serverCaps |= uint32(binary.LittleEndian.Uint16(p[3:])) << 16
		authLen := int(p[5])
		p = p[16:]
		if serverCaps&clientSecureConnection != 0 {
			n := min(max(13, authLen-8), len(p))
			part := p[:n]
			p = p[n:]
			if len(part) > 0 && part[len(part)-1] == 0 {
				part = part[:len(part)-1]
			}
			scramble = append(scramble, part...)
		}
		if serverCaps&clientPluginAuth != 0 && len(p) > 0 {
			if i := bytes.IndexByte(p, 0); i >= 0 {
				p = p[:i]
			}
			plugin = string(p)
		}
	}
	if serverCaps&clientProtocol41 == 0 {
		return errors.New("server does not support protocol 4.1")
	}

	want := uint32(clientLongPassword | clientLongFlag | clientConnectWithDB | clientProtocol41 |
		clientTransactions | clientSecureConnection | clientMultiResults | clientPluginAuth | clientPluginAuthLenencData)
	useTLS := cfg.tlsMode != "false"
	if useTLS {
		if serverCaps&clientSSL == 0 {
			return errors.New("tls is enabled in the configuration, but the server does not offer TLS")
		}
		want |= clientSSL
	}
	c.caps = want & serverCaps

	header := make([]byte, 32, 256)
	binary.LittleEndian.PutUint32(header[0:], c.caps)
	binary.LittleEndian.PutUint32(header[4:], maxPayload)
	header[8] = charsetUTF8MB4

	if useTLS {
		if err := c.writePacket(header); err != nil { // SSLRequest
			return err
		}
		tc := &tls.Config{ServerName: cfg.host, MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.tlsMode == "skip-verify"}
		if cfg.tlsCA != "" {
			pem, err := os.ReadFile(cfg.tlsCA)
			if err != nil {
				return err
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return fmt.Errorf("no certificates found in %s", cfg.tlsCA)
			}
			tc.RootCAs = pool
		}
		conn := tls.Client(c.nc, tc)
		if err := conn.Handshake(); err != nil {
			return fmt.Errorf("TLS handshake: %w", err)
		}
		c.nc, c.br = conn, bufio.NewReaderSize(conn, 64*1024)
	}
	secure := useTLS || unixSocket

	auth, err := authResponse(plugin, scramble, cfg.password, secure)
	if err != nil { // unknown default plugin: answer with native password, server may switch
		plugin = "mysql_native_password"
		if auth, err = authResponse(plugin, scramble, cfg.password, secure); err != nil {
			return err
		}
	}
	resp := append(header, cfg.user...)
	resp = append(resp, 0)
	if c.caps&clientPluginAuthLenencData != 0 {
		resp = appendLenEnc(resp, uint64(len(auth)))
	} else {
		resp = append(resp, byte(len(auth)))
	}
	resp = append(resp, auth...)
	if c.caps&clientConnectWithDB != 0 {
		resp = append(append(resp, cfg.database...), 0)
	}
	if c.caps&clientPluginAuth != 0 {
		resp = append(append(resp, plugin...), 0)
	}
	if err := c.writePacket(resp); err != nil {
		return err
	}

	for {
		pkt, err := c.readPacket()
		if err != nil {
			return err
		}
		switch pkt[0] {
		case 0x00:
			return nil
		case 0xff:
			return parseErrPacket(pkt)
		case 0xfe: // AuthSwitchRequest
			rest := pkt[1:]
			i := bytes.IndexByte(rest, 0)
			if i < 0 {
				return errors.New("server requested the old (pre-4.1) password protocol, which is not supported")
			}
			plugin = string(rest[:i])
			data := bytes.TrimSuffix(rest[i+1:], []byte{0})
			auth, err := authResponse(plugin, data, cfg.password, secure)
			if err != nil {
				return err
			}
			if err := c.writePacket(auth); err != nil {
				return err
			}
		default:
			return fmt.Errorf("authentication plugin %q needs a multi-step exchange that is not supported", plugin)
		}
	}
}

func authResponse(plugin string, scramble []byte, password string, secure bool) ([]byte, error) {
	switch plugin {
	case "mysql_native_password":
		if password == "" {
			return nil, nil
		}
		if len(scramble) < 20 {
			return nil, errors.New("server sent a short authentication scramble")
		}
		h1 := sha1.Sum([]byte(password))
		h2 := sha1.Sum(h1[:])
		h := sha1.New()
		h.Write(scramble[:20])
		h.Write(h2[:])
		out := h.Sum(nil)
		for i := range out {
			out[i] ^= h1[i]
		}
		return out, nil
	case "mysql_clear_password":
		if !secure {
			return nil, errors.New("server requests the clear-text password plugin; enable tls or use a unix socket")
		}
		return append([]byte(password), 0), nil
	case "unix_socket", "auth_socket":
		return nil, nil
	case "client_ed25519":
		return nil, errors.New("the account uses ed25519 authentication, which needs a non-stdlib library; use an account with mysql_native_password")
	}
	return nil, fmt.Errorf("unsupported authentication plugin %q", plugin)
}

// run sends one COM_QUERY and reads the complete answer.
func (c *conn) run(q []byte) (res okResult, cols []string, rows [][]*string, err error) {
	c.seq = 0
	cmd := make([]byte, 0, len(q)+1)
	cmd = append(append(cmd, comQuery), q...)
	if err = c.writePacket(cmd); err != nil {
		return
	}
	for {
		var more bool
		if res, cols, rows, more, err = c.readResult(); err != nil || !more {
			return
		}
	}
}

func (c *conn) readResult() (res okResult, cols []string, rows [][]*string, more bool, err error) {
	pkt, err := c.readPacket()
	if err != nil {
		return
	}
	switch pkt[0] {
	case 0x00:
		res, err = parseOK(pkt)
		return res, nil, nil, res.status&serverMoreResult != 0, err
	case 0xff:
		return res, nil, nil, false, parseErrPacket(pkt)
	case 0xfb:
		return res, nil, nil, false, errors.New("server requested LOCAL INFILE, which is not supported")
	}
	ncol, _, _, err := readLenEnc(pkt)
	if err != nil {
		return
	}
	cols = make([]string, ncol)
	for i := range cols {
		def, err := c.readPacket()
		if err != nil {
			return res, nil, nil, false, err
		}
		// catalog, schema, table, org_table, name
		p := def
		var name []byte
		for k := 0; k < 5 && err == nil; k++ {
			name, p, _, err = readLenEncString(p)
		}
		if err != nil {
			return res, nil, nil, false, err
		}
		cols[i] = string(name)
	}
	if pkt, err = c.readPacket(); err != nil {
		return
	}
	if !isEOFPacket(pkt) {
		return res, nil, nil, false, errMalformed
	}
	for {
		if pkt, err = c.readPacket(); err != nil {
			return
		}
		if pkt[0] == 0xff {
			return res, nil, nil, false, parseErrPacket(pkt)
		}
		if isEOFPacket(pkt) {
			if len(pkt) >= 5 {
				res.warnings = binary.LittleEndian.Uint16(pkt[1:])
				more = binary.LittleEndian.Uint16(pkt[3:])&serverMoreResult != 0
			}
			return res, cols, rows, more, nil
		}
		row := make([]*string, ncol)
		p := pkt
		for i := range row {
			var v []byte
			var null bool
			if v, p, null, err = readLenEncString(p); err != nil {
				return
			}
			if !null {
				s := string(v)
				row[i] = &s
			}
		}
		rows = append(rows, row)
	}
}

func (c *conn) exec(q any) (okResult, error) {
	var b []byte
	switch v := q.(type) {
	case string:
		b = []byte(v)
	case []byte:
		b = v
	}
	res, _, _, err := c.run(b)
	return res, err
}

func (c *conn) query(q string) ([]string, [][]*string, error) {
	_, cols, rows, err := c.run([]byte(q))
	return cols, rows, err
}

func (c *conn) close() {
	c.seq = 0
	c.writePacket([]byte{comQuit})
	c.nc.Close()
}
