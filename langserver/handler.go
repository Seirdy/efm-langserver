package langserver

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"

	"github.com/haya14busa/errorformat"
	"github.com/sourcegraph/jsonrpc2"
)

type Config struct {
	LintErrorFormats []string `yaml:"lint-error-formats"`
	LintStdin        bool     `yaml:"lint-stdin"`
	LintOffset       int      `yaml:"lint-offset"`
	LintCommand      string   `yaml:"lint-command"`
}

func NewHandler(configs map[string]Config) jsonrpc2.Handler {
	for _, v := range configs {
		if v.LintErrorFormats == nil || len(v.LintErrorFormats) == -1 {
			v.LintErrorFormats = []string{"%f:%l:%m", "%f:%l:%c:%m"}
		}
	}
	// TODO Add formatCommand
	var handler = &langHandler{
		configs: configs,
		files:   make(map[string]*File),
		request: make(chan string),
		conn:    nil,
	}
	go handler.linter()
	return jsonrpc2.HandlerWithError(handler.handle)
}

type langHandler struct {
	configs map[string]Config
	files   map[string]*File
	request chan string
	conn    *jsonrpc2.Conn
}

type File struct {
	LanguageId string
	Text       string
}

func isWindowsDrivePath(path string) bool {
	if len(path) < 4 {
		return false
	}
	return unicode.IsLetter(rune(path[0])) && path[1] == ':'
}

func isWindowsDriveURI(uri string) bool {
	if len(uri) < 4 {
		return false
	}
	return uri[0] == '/' && unicode.IsLetter(rune(uri[1])) && uri[2] == ':'
}

func fromURI(uri string) (string, error) {
	u, err := url.ParseRequestURI(uri)
	if err != nil {
		return "", err
	}
	if u.Scheme != "file" {
		return "", fmt.Errorf("only file URIs are supported, got %v", u.Scheme)
	}
	if isWindowsDriveURI(u.Path) {
		u.Path = u.Path[1:]
	}
	return u.Path, nil
}

func toURI(path string) *url.URL {
	if isWindowsDrivePath(path) {
		path = "/" + path
	}
	return &url.URL{
		Scheme: "file",
		Path:   filepath.ToSlash(path),
	}
}

func (h *langHandler) linter() {
	for {
		uri, ok := <-h.request
		if !ok {
			break
		}
		h.conn.Notify(
			context.Background(),
			"textDocument/publishDiagnostics",
			&PublishDiagnosticsParams{
				URI:         uri,
				Diagnostics: h.lint(uri),
			})
	}
}

func (h *langHandler) lint(uri string) []Diagnostic {
	f, ok := h.files[uri]
	if !ok {
		fmt.Fprintf(os.Stderr, "document not found")
		return nil
	}

	fname, err := fromURI(uri)
	if err != nil {
		fmt.Fprint(os.Stderr, err)
		return nil
	}
	fname = filepath.ToSlash(fname)
	if runtime.GOOS == "windows" {
		fname = strings.ToLower(fname)
	}

	config := h.configFor(uri)

	efms, err := errorformat.NewErrorformat(config.LintErrorFormats)
	if err != nil {
		fmt.Fprint(os.Stderr, err)
		return nil
	}
	diagnostics := []Diagnostic{}

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", config.LintCommand)
	} else {
		cmd = exec.Command("sh", "-c", config.LintCommand)
	}
	if config.LintStdin {
		cmd.Stdin = strings.NewReader(f.Text)
	}
	b, err := cmd.CombinedOutput()
	if err == nil {
		fmt.Fprintf(os.Stderr, "succeeded: %q", f.Text)
		return diagnostics
	}
	for _, line := range strings.Split(string(b), "\n") {
		for _, ef := range efms.Efms {
			m := ef.Match(string(line))
			if m == nil {
				continue
			}
			if config.LintStdin && (m.F == "stdin" || m.F == "-") {
				m.F = fname
			} else {
				m.F = filepath.ToSlash(m.F)
			}
			if m.C == 0 {
				m.C = 1
			}
			path, err := filepath.Abs(m.F)
			if err != nil {
				continue
			}
			path = filepath.ToSlash(path)
			if runtime.GOOS == "windows" {
				path = strings.ToLower(path)
			}
			if path != fname {
				continue
			}
			diagnostics = append(diagnostics, Diagnostic{
				Range: Range{
					Start: Position{Line: m.L - 1 - config.LintOffset, Character: m.C - 1},
					End:   Position{Line: m.L - 1 - config.LintOffset, Character: m.C - 1},
				},
				Message:  m.M,
				Severity: 1,
			})
		}
	}

	return diagnostics
}

func (h *langHandler) closeFile(uri string) error {
	delete(h.files, uri)
	return nil
}

func (h *langHandler) saveFile(uri string) error {
	h.request <- uri
	return nil
}

func (h *langHandler) openFile(uri string, languageId string) error {
	f := &File{
		Text:       "",
		LanguageId: languageId,
	}
	h.files[uri] = f
	return nil
}

func (h *langHandler) updateFile(uri string, text string) error {
	f, ok := h.files[uri]
	if !ok {
		return fmt.Errorf("document not found: %v", uri)
	}
	f.Text = text

	h.request <- uri
	return nil
}

func (h *langHandler) handle(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) (result interface{}, err error) {
	switch req.Method {
	case "initialize":
		return h.handleInitialize(ctx, conn, req)
	case "shutdown":
		return h.handleShutdown(ctx, conn, req)
	case "textDocument/didOpen":
		return h.handleTextDocumentDidOpen(ctx, conn, req)
	case "textDocument/didChange":
		return h.handleTextDocumentDidChange(ctx, conn, req)
	case "textDocument/didSave":
		return h.handleTextDocumentDidSave(ctx, conn, req)
	case "textDocument/didClose":
		return h.handleTextDocumentDidClose(ctx, conn, req)
	}

	return nil, &jsonrpc2.Error{Code: jsonrpc2.CodeMethodNotFound, Message: fmt.Sprintf("method not supported: %s", req.Method)}
}

func (h *langHandler) configFor(uri string) Config {
	f, ok := h.files[uri]
	if !ok {
		return Config{}
	}
	return h.configs[f.LanguageId]
}
