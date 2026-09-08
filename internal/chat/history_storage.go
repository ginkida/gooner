package chat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// Keep IDs below common 255-byte path-component limits after adding the
	// .json suffix. The rune limit preserves the existing slash-command UX.
	MaxSessionIDRunes = 120
	maxSessionIDBytes = 240

	// Persisted state is untrusted input on load. Byte limits prevent a single
	// file from consuming unbounded memory; structural limits prevent compact
	// JSON such as millions of empty array elements from expanding far beyond
	// its on-disk size during typed unmarshalling.
	maxLegacyHistoryFileBytes  int64 = 8 << 20
	maxSessionFileBytes        int64 = 16 << 20
	maxJSONDepth                     = 64
	maxJSONContainerEntries          = 4096
	maxJSONValues                    = 250_000
	maxSessionDirectoryEntries       = 10_000
)

// ValidateSessionID enforces one portable storage and CLI identity contract.
// It rejects path separators, Windows alternate-data-stream/device names,
// terminal/control characters, and leading option-like names. Unicode names
// remain supported as long as they fit portable path-component limits.
func ValidateSessionID(id string) error {
	if id == "" || id == "." || id == ".." {
		return fmt.Errorf("invalid session ID %q", id)
	}
	if !utf8.ValidString(id) || utf8.RuneCountInString(id) > MaxSessionIDRunes || len(id) > maxSessionIDBytes {
		return fmt.Errorf("invalid session ID %q: must be valid UTF-8 and at most %d characters/%d bytes", id, MaxSessionIDRunes, maxSessionIDBytes)
	}
	if strings.HasPrefix(id, "-") || strings.HasSuffix(id, ".") || strings.HasSuffix(id, " ") {
		return fmt.Errorf("invalid session ID %q", id)
	}
	if strings.ContainsAny(id, `<>:"/\|?*`) {
		return fmt.Errorf("invalid session ID %q: contains a non-portable filename character", id)
	}
	if strings.IndexFunc(id, func(r rune) bool {
		return unicode.IsControl(r) || unicode.IsSpace(r)
	}) >= 0 {
		return fmt.Errorf("invalid session ID %q: contains whitespace or a control character", id)
	}

	// Windows reserves these device basenames even when an extension follows
	// (for example CON.json). Check the portion before the first dot.
	base := id
	if dot := strings.IndexByte(base, '.'); dot >= 0 {
		base = base[:dot]
	}
	switch upper := strings.ToUpper(base); upper {
	case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		return fmt.Errorf("invalid session ID %q: reserved device name", id)
	}
	return nil
}

// sessionJSONPath binds a session ID to one flat JSON file inside dir.
func sessionJSONPath(dir, sessionID string) (string, error) {
	if err := ValidateSessionID(sessionID); err != nil {
		return "", err
	}
	return filepath.Join(dir, sessionID+".json"), nil
}

// openPrivateDir creates (when requested), verifies, and opens a real
// directory without chmod-following a final-component symlink. The returned
// descriptor remains bound to the verified directory across later path swaps.
func openPrivateDir(path string, create bool) (*os.File, error) {
	if create {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return nil, err
		}
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("session storage path %q is not a real directory", path)
	}
	dir, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = dir.Close()
		}
	}()
	opened, err := dir.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.IsDir() || !os.SameFile(before, opened) {
		return nil, fmt.Errorf("session storage directory changed while opening")
	}
	if err := dir.Chmod(0o700); err != nil {
		return nil, fmt.Errorf("set session storage directory permissions: %w", err)
	}
	ok = true
	return dir, nil
}

func ensurePrivateDir(path string) error {
	dir, err := openPrivateDir(path, true)
	if err != nil {
		return err
	}
	return dir.Close()
}

// openVerifiedRegular rejects symlinks and non-regular files before any data
// is read. Comparing Lstat with the opened descriptor closes the usual
// symlink-swap window without relying on platform-specific O_NOFOLLOW flags.
func openVerifiedRegular(path string) (*os.File, os.FileInfo, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("session path %q is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	opened, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) || opened.Size() != before.Size() || !opened.ModTime().Equal(before.ModTime()) {
		_ = f.Close()
		return nil, nil, fmt.Errorf("session file changed while opening")
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("set session file permissions: %w", err)
	}
	return f, opened, nil
}

func readPrivateRegularFile(path string, maxBytes int64) ([]byte, error) {
	f, opened, err := openVerifiedRegular(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if opened.Size() < 0 || opened.Size() > maxBytes {
		return nil, fmt.Errorf("%w: %d bytes (maximum %d)", errSessionFileTooLarge, opened.Size(), maxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: exceeds %d bytes", errSessionFileTooLarge, maxBytes)
	}
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(opened, after) || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) || int64(len(data)) != after.Size() {
		return nil, fmt.Errorf("session file changed while reading")
	}
	return data, nil
}

// preparePrivateWriteTarget refuses to overwrite a symlink or special file.
// AtomicWrite will publish a new 0600 inode, while this hardens an existing
// regular target even if serialization later fails.
func preparePrivateWriteTarget(path string) error {
	f, _, err := openVerifiedRegular(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return f.Close()
}

func removePrivateRegular(path string) error {
	f, _, err := openVerifiedRegular(path)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Remove(path)
}

func scanJSONFiles(dirPath string) ([]string, error) {
	dir, err := openPrivateDir(dirPath, true)
	if err != nil {
		return nil, err
	}
	defer dir.Close()

	var names []string
	visited := 0
	for {
		entries, readErr := dir.ReadDir(256)
		for _, entry := range entries {
			visited++
			if visited > maxSessionDirectoryEntries {
				return nil, fmt.Errorf("session directory contains more than %d entries", maxSessionDirectoryEntries)
			}
			name := entry.Name()
			if filepath.Ext(name) != ".json" || entry.Type()&os.ModeSymlink != 0 {
				continue
			}
			info, infoErr := entry.Info()
			if infoErr != nil || !info.Mode().IsRegular() {
				continue
			}
			id := strings.TrimSuffix(name, ".json")
			if ValidateSessionID(id) != nil {
				continue
			}
			names = append(names, name)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	sort.Strings(names)
	return names, nil
}

func validateBoundedJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	values := 0
	if err := validateJSONValue(dec, 0, &values); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

// validateJSONPayloadForSave bounds live state before encoding/json allocates
// its output buffer. The post-marshal byte/structure checks remain the exact
// authority; this preflight prevents a gigantic in-memory message or cyclic /
// highly-expanded map from causing an avoidable allocation spike first.
func validateJSONPayloadForSave(value any, maxBytes int64) error {
	walk := jsonPayloadWalk{
		remaining: maxBytes,
		limit:     maxBytes,
		active:    make(map[payloadVisit]bool),
	}
	if err := walk.value(reflect.ValueOf(value), 0); err != nil {
		return err
	}
	return nil
}

type payloadVisit struct {
	typ reflect.Type
	ptr uintptr
}

type jsonPayloadWalk struct {
	remaining int64
	limit     int64
	values    int
	active    map[payloadVisit]bool
}

var persistedTimeType = reflect.TypeOf(time.Time{})

func (w *jsonPayloadWalk) consume(bytes int64) error {
	if bytes < 0 || bytes > w.remaining {
		return fmt.Errorf("%w: JSON payload over %d bytes", ErrSessionStateTooLarge, w.limit)
	}
	w.remaining -= bytes
	return nil
}

func (w *jsonPayloadWalk) value(v reflect.Value, depth int) error {
	if depth > maxJSONDepth {
		return fmt.Errorf("JSON nesting exceeds %d levels", maxJSONDepth)
	}
	w.values++
	if w.values > maxJSONValues {
		return fmt.Errorf("JSON contains more than %d values", maxJSONValues)
	}
	// MarshalIndent adds a newline plus two spaces per nesting level around
	// container values. Charge that overhead up front so deeply nested but
	// otherwise tiny live state cannot evade the pre-allocation budget.
	if err := w.consume(int64(depth*2 + 2)); err != nil {
		return err
	}
	if !v.IsValid() {
		return w.consume(4) // null
	}
	for v.Kind() == reflect.Interface {
		if v.IsNil() {
			return w.consume(4)
		}
		v = v.Elem()
	}

	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return w.consume(4)
		}
		visit := payloadVisit{typ: v.Type(), ptr: v.Pointer()}
		if w.active[visit] {
			return fmt.Errorf("session JSON payload contains a cycle")
		}
		w.active[visit] = true
		defer delete(w.active, visit)
		return w.value(v.Elem(), depth+1)
	case reflect.String:
		return w.consume(jsonStringEncodedLen(v.String()))
	case reflect.Bool:
		return w.consume(5)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return w.consume(32)
	case reflect.Slice:
		if v.IsNil() {
			return w.consume(4)
		}
		if v.Type().Elem().Kind() == reflect.Uint8 {
			// encoding/json base64-encodes ordinary byte slices. RawMessage may be
			// smaller, so this is intentionally conservative for preflight only.
			return w.consume(int64((v.Len()+2)/3*4 + 2))
		}
		fallthrough
	case reflect.Array:
		if v.Len() > maxJSONContainerEntries {
			return fmt.Errorf("JSON array contains more than %d elements", maxJSONContainerEntries)
		}
		if err := w.consume(int64(v.Len() + 2)); err != nil {
			return err
		}
		var visit payloadVisit
		if v.Kind() == reflect.Slice && v.Len() > 0 {
			visit = payloadVisit{typ: v.Type(), ptr: v.Pointer()}
			if w.active[visit] {
				return fmt.Errorf("session JSON payload contains a cycle")
			}
			w.active[visit] = true
			defer delete(w.active, visit)
		}
		for i := 0; i < v.Len(); i++ {
			if err := w.value(v.Index(i), depth+1); err != nil {
				return err
			}
		}
		return nil
	case reflect.Map:
		if v.IsNil() {
			return w.consume(4)
		}
		if v.Len() > maxJSONContainerEntries {
			return fmt.Errorf("JSON object contains more than %d fields", maxJSONContainerEntries)
		}
		if err := w.consume(int64(v.Len() + 2)); err != nil {
			return err
		}
		visit := payloadVisit{typ: v.Type(), ptr: v.Pointer()}
		if w.active[visit] {
			return fmt.Errorf("session JSON payload contains a cycle")
		}
		w.active[visit] = true
		defer delete(w.active, visit)
		iter := v.MapRange()
		for iter.Next() {
			if err := w.value(iter.Key(), depth+1); err != nil {
				return err
			}
			if err := w.value(iter.Value(), depth+1); err != nil {
				return err
			}
		}
		return nil
	case reflect.Struct:
		if v.Type() == persistedTimeType {
			return w.consume(40)
		}
		if err := w.consume(2); err != nil {
			return err
		}
		typ := v.Type()
		for i := 0; i < v.NumField(); i++ {
			field := typ.Field(i)
			if field.PkgPath != "" || field.Tag.Get("json") == "-" {
				continue
			}
			name := field.Name
			if tagName := strings.Split(field.Tag.Get("json"), ",")[0]; tagName != "" {
				name = tagName
			}
			if err := w.consume(jsonStringEncodedLen(name)); err != nil {
				return err
			}
			if err := w.value(v.Field(i), depth+1); err != nil {
				return err
			}
		}
		return nil
	case reflect.Invalid:
		return w.consume(4)
	default:
		return fmt.Errorf("session JSON payload contains unsupported %s", v.Kind())
	}
}

func jsonStringEncodedLen(value string) int64 {
	length := int64(2) // surrounding quotes
	for i := 0; i < len(value); {
		b := value[i]
		if b < utf8.RuneSelf {
			switch {
			case b == '\\' || b == '"' || b == '\b' || b == '\f' || b == '\n' || b == '\r' || b == '\t':
				length += 2
			case b < 0x20 || b == '<' || b == '>' || b == '&':
				length += 6
			default:
				length++
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(value[i:])
		if r == utf8.RuneError && size == 1 {
			length += 6
			i++
			continue
		}
		if r == '\u2028' || r == '\u2029' {
			length += 6
		} else {
			length += int64(size)
		}
		i += size
	}
	return length
}

func validateJSONValue(dec *json.Decoder, depth int, values *int) error {
	if depth > maxJSONDepth {
		return fmt.Errorf("JSON nesting exceeds %d levels", maxJSONDepth)
	}
	(*values)++
	if *values > maxJSONValues {
		return fmt.Errorf("JSON contains more than %d values", maxJSONValues)
	}
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '[':
		count := 0
		for dec.More() {
			count++
			if count > maxJSONContainerEntries {
				return fmt.Errorf("JSON array contains more than %d elements", maxJSONContainerEntries)
			}
			if err := validateJSONValue(dec, depth+1, values); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil || end != json.Delim(']') {
			return fmt.Errorf("invalid JSON array terminator")
		}
	case '{':
		count := 0
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return err
			}
			if _, ok := key.(string); !ok {
				return fmt.Errorf("invalid JSON object key")
			}
			count++
			if count > maxJSONContainerEntries {
				return fmt.Errorf("JSON object contains more than %d fields", maxJSONContainerEntries)
			}
			if err := validateJSONValue(dec, depth+1, values); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil || end != json.Delim('}') {
			return fmt.Errorf("invalid JSON object terminator")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
	return nil
}
